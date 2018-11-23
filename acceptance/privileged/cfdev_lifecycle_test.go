package privileged_test

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/kinesis"
	"github.com/harlow/kinesis-consumer"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	"github.com/onsi/gomega/gexec"
	"net"
	"os"
	"os/exec"
	"strconv"
	"time"

	"io/ioutil"
	"net/http"

	. "code.cloudfoundry.org/cfdev/acceptance"
	"github.com/cloudfoundry-incubator/cf-test-helpers/cf"
	"github.com/onsi/gomega/gbytes"
)

var analyticsReceived = make(map[string]int)

var _ = Describe("cfdev lifecycle", func() {

	var (
		startSession *gexec.Session
	)

	BeforeEach(func() {
		// stop should succeed even when nothing is running
		stopSession := cf.Cf("dev", "stop")
		Eventually(stopSession).Should(gexec.Exit(0))

		if tarballPath := os.Getenv("TARBALL_PATH"); tarballPath != "" {
			startSession = cf.Cf("dev", "start", "-f", tarballPath)
		} else {
			startSession = cf.Cf("dev", "start")
		}
	})

	AfterEach(func() {
		if os.Getenv("CLEANUP") == "false" {
			fmt.Fprint(GinkgoWriter, "'CLEANUP=false' environment variable detected, skipping tear-down...")
			return
		}

		telemetrySession := cf.Cf("dev", "telemetry", "--off")
		Eventually(telemetrySession).Should(gexec.Exit())

		startSession.Kill()

		stopSession := cf.Cf("dev", "stop")
		Eventually(stopSession).Should(gexec.Exit(0))

		// check that VM is removed by stop command
		if IsWindows() {
			Expect(doesVMExist()).To(BeFalse())
		} else {
			Eventually(IsLaunchdRunning("org.cloudfoundry.cfdev.linuxkit")).Should(BeFalse())
			Eventually(IsLaunchdRunning("org.cloudfoundry.cfdev.vpnkit")).Should(BeFalse())
		}
	})

	It("runs the entire vm lifecycle", func() {
		//userID, _ := machineid.ProtectedID("cfdev")
		appCreatedEventName := "app created"
		telemetryOffEventName := "telemetry off"
		analyticsReceived[telemetryOffEventName] = 0
		analyticsReceived[appCreatedEventName] = 0

		//go streamKinesis(userID)

		By("waiting for bosh to deploy")
		Eventually(startSession, 2*time.Hour).Should(gbytes.Say("Deploying the BOSH Director"))

		EventuallyWeCanTargetTheBOSHDirector()

		By("waiting for cfdev cli to exit when the deploy finished")
		Eventually(startSession, 3*time.Hour).Should(gexec.Exit(0))

		By("waiting for cf router to listen")
		loginSession := cf.Cf("login", "-a", "https://api.dev.cfdev.sh", "--skip-ssl-validation", "-u", "admin", "-p", "admin", "-o", "cfdev-org", "-s", "cfdev-space")
		Eventually(loginSession).Should(gexec.Exit(0))

		By("toggling off telemetry")
		telemetrySession := cf.Cf("dev", "telemetry", "--off")
		Eventually(telemetrySession).Should(gexec.Exit(0))
		Eventually(IsLaunchdRunning("org.cloudfoundry.cfdev.cfanalyticsd")).Should(BeFalse())

		By("toggling telemetry on")
		telemetrySession = cf.Cf("dev", "telemetry", "--on")
		Eventually(telemetrySession).Should(gexec.Exit(0))
		Eventually(IsLaunchdRunning("org.cloudfoundry.cfdev.cfanalyticsd")).Should(BeTrue())
		time.Sleep(10 * time.Second)

		By("pushing an app")
		PushAnApp()

		//Eventually(func() bool {
		//	return analyticsReceived[appCreatedEventName] == 1
		//}, 5*time.Minute, 2*time.Second).Should(BeTrue())
		//
		//Eventually(func() bool {
		//	return analyticsReceived[telemetryOffEventName] == 1
		//}, 5*time.Minute, 2*time.Second).Should(BeTrue())

		By("rerunning cf dev start")
		startSession = cf.Cf("dev", "start")
		Eventually(startSession).Should(gbytes.Say("CF Dev is already running..."))

		By("checking for cf versions")
		var versionSession *gexec.Session

		if tarballPath := os.Getenv("TARBALL_PATH"); tarballPath != "" {
			versionSession = cf.Cf("dev", "version", "-f", tarballPath)
		} else {
			versionSession = cf.Cf("dev", "version")
		}

		Eventually(versionSession).Should(gexec.Exit(0))
		Expect(string(versionSession.Out.Contents())).To(ContainSubstring("CLI:"))
		Expect(string(versionSession.Out.Contents())).To(ContainSubstring("cf:"))
	})
})

func EventuallyWeCanTargetTheBOSHDirector() {
	By("waiting for bosh to listen")

	Eventually(func() error {
		return HttpServerIsListeningAt("https://" + BoshDirectorIP + ":25555")
	}, 15*time.Minute, 30*time.Second).ShouldNot(HaveOccurred())

	w := gexec.NewPrefixedWriter("[bosh env] ", GinkgoWriter)
	Eventually(func() error {

		var boshCmd *exec.Cmd

		if IsWindows() {
			boshCmd = exec.Command("powershell.exe",
				"-Command",
				`cf dev bosh env | Invoke-Expression; bosh env`)
		} else {
			boshCmd = exec.Command("/bin/sh",
				"-e",
				"-c", `eval "$(cf dev bosh env)" && bosh env`)
		}

		output, err := boshCmd.CombinedOutput()
		fmt.Fprintln(w, string(output))
		return err
	}, 10*time.Minute, 30*time.Second).Should(BeNil())
}

func PushAnApp() {
	server, port := fakeTcpServer()
	defer server.Close()

	By("pushing app")

	Eventually(cf.Cf("push", "cf-test-app", "--no-start", "-p", "./fixture", "-b", "ruby_buildpack")).Should(gexec.Exit(0))
	Eventually(cf.Cf("set-env", "cf-test-app", "HOST_SERVER_PORT", strconv.Itoa(port))).Should(gexec.Exit(0))
	Eventually(cf.Cf("create-service", "p-mysql", "10mb", "mydb")).Should(gexec.Exit(0))
	Eventually(cf.Cf("bind-service", "cf-test-app", "mydb")).Should(gexec.Exit(0))
	Eventually(cf.Cf("start", "cf-test-app"), 10*time.Minute).Should(gexec.Exit(0))

	Expect(httpGet("http://cf-test-app.dev.cfdev.sh")).To(Equal("Hello, world!"))
	Expect(httpGet("http://cf-test-app.dev.cfdev.sh/external")).To(ContainSubstring("Example Domain"))
	Expect(httpGet("http://cf-test-app.dev.cfdev.sh/host")).To(Equal("Text From Test Code"))
	Expect(httpGet("http://cf-test-app.dev.cfdev.sh/mysql")).To(ContainSubstring("innodb"))

	Eventually(cf.Cf("create-shared-domain", "tcp.dev.cfdev.sh", "--router-group", "default-tcp")).Should(gexec.Exit(0))
	Eventually(cf.Cf("create-route", "cfdev-space", "tcp.dev.cfdev.sh", "--port", "1030")).Should(gexec.Exit(0))
	Eventually(cf.Cf("map-route", "cf-test-app", "tcp.dev.cfdev.sh", "--port", "1030")).Should(gexec.Exit(0))

	Eventually(func() (string, error) {
		return httpGet("http://tcp.dev.cfdev.sh:1030")
	}).Should(Equal("Hello, world!"))
}

func fakeTcpServer() (net.Listener, int) {
	server, err := net.Listen("tcp", "localhost:0")
	Expect(err).NotTo(HaveOccurred())
	go func() {
		for {
			conn, err := server.Accept()
			if err != nil {
				continue
			}
			_, err = conn.Write([]byte("Text From Test Code"))
			Expect(err).NotTo(HaveOccurred())
			conn.Close()
		}
	}()
	return server, server.Addr().(*net.TCPAddr).Port
}

func httpGet(url string) (string, error) {
	resp, err := http.Get(url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, err := ioutil.ReadAll(resp.Body)
	return string(b), err
}

func doesVMExist() bool {
	cmd := exec.Command("powershell.exe", "-Command", "(Get-VM -Name cfdev).name")
	output, err := cmd.Output()
	if err != nil {
		return false
	}

	return string(output) == "cfdev"
}

type StatMessage struct {
	UserId    string `json:"userId"`
	Event     string `json:"event"`
	Timestamp string `json:"timestamp"`
}

func streamKinesis(userId string) {
	accessKeyID := os.Getenv("AWS_ACCESS_KEY_ID")
	secretAccessKey := os.Getenv("AWS_SECRET_ACCESS_KEY")

	if accessKeyID == "" || secretAccessKey == "" {
		fmt.Fprintln(GinkgoWriter, "AWS keys not detected. Skipping assertions for analytics...")
		return
	}

	stream := flag.String("cfdev-analytics-development", "cfdev-analytics-development", "cfdev-analytics-development")
	flag.Parse()

	myKinesisClient := kinesis.New(session.New(aws.NewConfig()), &aws.Config{
		Region:      aws.String("us-east-1"),
		Credentials: credentials.NewStaticCredentials(accessKeyID, secretAccessKey, ""),
	})
	newKclient, err := consumer.NewKinesisClient(consumer.WithKinesis(myKinesisClient))
	c, err := consumer.New(
		*stream,
		consumer.WithClient(newKclient),
	)
	if err != nil {
		fmt.Printf("consumer error: %v \n", err)
	}
	ctx, _ := context.WithCancel(context.Background())
	err = c.Scan(ctx, func(r *consumer.Record) consumer.ScanError {
		var analyticsEvent StatMessage
		json.Unmarshal(r.Data, &analyticsEvent)
		eventTime, err := time.Parse(time.RFC3339, analyticsEvent.Timestamp)
		tenMinutesAgo := time.Now().UTC().Add(-10 * time.Minute)
		if eventTime.After(tenMinutesAgo) {
			if _, ok := analyticsReceived[analyticsEvent.Event]; ok {
				if analyticsEvent.UserId == userId {
					analyticsReceived[analyticsEvent.Event]++
				}
			}
		}
		err = errors.New("some error happened")

		return consumer.ScanError{
			Error:          err,
			StopScan:       false,
			SkipCheckpoint: false,
		}
	})
}
