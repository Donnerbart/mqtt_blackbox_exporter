package main

import (
	"crypto/tls"
	"crypto/x509"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"math/rand"
	"net/http"
	"os"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"gopkg.in/yaml.v2"
)

type config struct {
	Probes []probeConfig `yaml:"probes"`
}

type probeConfig struct {
	Name               string        `yaml:"name"`
	Broker             string        `yaml:"broker_url"`
	SubscribeTopic     string        `yaml:"subscribe_topic"`
	Topic              string        `yaml:"topic"`
	ClientPrefix       string        `yaml:"client_prefix"`
	Username           string        `yaml:"username"`
	Password           string        `yaml:"password"`
	ClientCert         string        `yaml:"client_cert"`
	ClientKey          string        `yaml:"client_key"`
	CAChain            string        `yaml:"ca_chain"`
	InsecureSkipVerify bool          `yaml:"insecure_skip_verify"`
	Messages           int           `yaml:"messages"`
	TestInterval       time.Duration `yaml:"interval"`
	MessagePayload     string        `yaml:"message_payload"`
}

var build string

var (
	messagesPublished = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "probe_mqtt_messages_published_total",
			Help: "Number of published messages.",
		}, []string{"name", "broker"})

	messagesPublishTimeout = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "probe_mqtt_messages_publish_timeout_total",
			Help: "Number of published messages.",
		}, []string{"name", "broker"})

	messagesReceived = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "probe_mqtt_messages_received_total",
			Help: "Number of received messages.",
		}, []string{"name", "broker"})

	timedoutTests = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "probe_mqtt_timeouts_total",
			Help: "Number of timed out tests.",
		}, []string{"name", "broker"})

	probeStarted = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "probe_mqtt_started_total",
			Help: "Number of started probes.",
		}, []string{"name", "broker"})

	probeCompleted = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "probe_mqtt_completed_total",
			Help: "Number of completed probes.",
		}, []string{"name", "broker"})

	errors = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "probe_mqtt_errors_total",
			Help: "Number of errors occurred during test execution.",
		}, []string{"name", "broker"})

	probeDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name: "probe_mqtt_duration_seconds",
			Help: "Time taken to execute probe.",
		}, []string{"name", "broker"})

	logger = log.New(os.Stderr, "", log.Lmicroseconds|log.Ltime|log.Lshortfile)

	configFile    = flag.String("config.file", "config.yaml", "Exporter configuration file.")
	listenAddress = flag.String("web.listen-address", ":9214", "The address to listen on for HTTP requests.")
	enableDebug   = flag.Bool("debug.enable", false, "set this flag to enable exporter debugging")
	enableTrace   = flag.Bool("trace.enable", false, "set this flag to enable mqtt tracing")
)

var letterRunes = []rune("abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ")

func init() {
	prometheus.MustRegister(probeStarted)
	prometheus.MustRegister(probeDuration)
	prometheus.MustRegister(probeCompleted)
	prometheus.MustRegister(messagesPublished)
	prometheus.MustRegister(messagesReceived)
	prometheus.MustRegister(messagesPublishTimeout)
	prometheus.MustRegister(timedoutTests)
	prometheus.MustRegister(errors)
}

func RandStringRunes(n int) string {
	b := make([]rune, n)
	for i := range b {
		b[i] = letterRunes[rand.Intn(len(letterRunes))]
	}
	return string(b)
}

// newTLSConfig sets up the go internal tls config from the given probe config.
func newTLSConfig(probeConfig *probeConfig) (*tls.Config, error) {
	cfg := &tls.Config{
		// RootCAs = certs used to verify server cert.
		RootCAs: nil,
		// ClientAuth = whether to request cert from server.
		// Since the server is set up for SSL, this happens
		// anyway.
		ClientAuth: tls.NoClientCert,
		// InsecureSkipVerify = verify that cert contents
		// match server. IP matches what is in cert etc.
		// If you set this to true, you basically trust any server
		// presenting an SSL cert to you and rendering SSL useless.
		InsecureSkipVerify: probeConfig.InsecureSkipVerify,
		// Certificates = list of certs client sends to server.
		Certificates: nil,
	}
	// Import trusted certificates from CAChain - purely for verification - not sent to TLS server
	if probeConfig.CAChain != "" {
		certpool := x509.NewCertPool()
		pemCerts, err := ioutil.ReadFile(probeConfig.CAChain)
		if err != nil {
			return nil, fmt.Errorf("could not read ca_chain pem: %s", err.Error())
		}
		certpool.AppendCertsFromPEM(pemCerts)
		cfg.RootCAs = certpool
	}

	if probeConfig.ClientCert != "" && probeConfig.ClientKey != "" {
		// Import client certificate/key pair
		// If you want the chain certs to be sent to the server, concatenate the leaf,
		//  intermediate and root into the ClientCert file
		cert, err := tls.LoadX509KeyPair(probeConfig.ClientCert, probeConfig.ClientKey)
		if err != nil {
			return nil, fmt.Errorf("could not read client certificate an key: %s", err.Error())
		}
		cfg.Certificates = []tls.Certificate{cert}
	}

	if (probeConfig.ClientCert != "" && probeConfig.ClientKey == "") ||
		(probeConfig.ClientCert == "" && probeConfig.ClientKey != "") {
		return nil, fmt.Errorf("either ClientCert or ClientKey is set to empty string")
	}

	return cfg, nil
}

func connectClient(probeConfig *probeConfig, timeout time.Duration, opts *mqtt.ClientOptions) (mqtt.Client, error) {
	tlsConfig, err := newTLSConfig(probeConfig)
	if err != nil {
		return nil, fmt.Errorf("could not setup TLS: %s", err.Error())
	}
	baseOptions := mqtt.NewClientOptions()
	if opts != nil {
		baseOptions = opts
	}
	baseOptions = baseOptions.SetAutoReconnect(false).
		SetUsername(probeConfig.Username).
		SetPassword(probeConfig.Password).
		SetTLSConfig(tlsConfig).
		AddBroker(probeConfig.Broker)
	baseOptions.SetConnectionLostHandler(func(c mqtt.Client, err error) {
		logger.Printf("Probe %s: lost MQTT connection to %s error: %s", probeConfig.Name, probeConfig.Broker, err.Error())
	})

	client := mqtt.NewClient(baseOptions)
	token := client.Connect()
	success := token.WaitTimeout(timeout)
	if !success {
		return nil, fmt.Errorf("reached connect timeout")
	}
	if token.Error() != nil {
		return nil, fmt.Errorf("failed to connect client: %s", token.Error().Error())
	}
	return client, nil
}

func startProbe(probeConfig *probeConfig) {
	num := probeConfig.Messages
	minTimeout := 10 * time.Second
	setupTimeout := probeConfig.TestInterval / 3
	if setupTimeout < minTimeout {
		setupTimeout = minTimeout
	}
	probeTimeout := probeConfig.TestInterval / 3
	if probeTimeout < minTimeout {
		probeTimeout = minTimeout
	}
	qos := byte(0)
	t0 := time.Now()
	setupDeadLine := t0.Add(setupTimeout)

	// Initialize optional metrics with initial values to have them present from the beginning
	messagesPublished.WithLabelValues(probeConfig.Name, probeConfig.Broker).Add(0)
	messagesPublishTimeout.WithLabelValues(probeConfig.Name, probeConfig.Broker).Add(0)
	messagesReceived.WithLabelValues(probeConfig.Name, probeConfig.Broker).Add(0)
	timedoutTests.WithLabelValues(probeConfig.Name, probeConfig.Broker).Add(0)
	errors.WithLabelValues(probeConfig.Name, probeConfig.Broker).Add(0)

	// Starting to fill metric vectors with meaningful values
	probeStarted.WithLabelValues(probeConfig.Name, probeConfig.Broker).Inc()
	defer func() {
		elapsed := time.Since(t0)
		probeCompleted.WithLabelValues(probeConfig.Name, probeConfig.Broker).Inc()
		probeDuration.WithLabelValues(probeConfig.Name, probeConfig.Broker).Observe(elapsed.Seconds())
		if *enableDebug {
			logger.Printf("Probe %s: took %d ms", probeConfig.Name, elapsed.Milliseconds())
		}
	}()

	queue := make(chan [2]string)
	reportError := func(label string, error error) {
		errors.WithLabelValues(probeConfig.Name, probeConfig.Broker).Inc()
		logger.Printf("Probe %s: %s -> %s", probeConfig.Name, label, error.Error())
	}

	clientSuffix := RandStringRunes(5)

	publisherOptions := mqtt.NewClientOptions().
		SetClientID(fmt.Sprintf("%s-p-%s", probeConfig.ClientPrefix, clientSuffix))

	subscriberOptions := mqtt.NewClientOptions().
		SetClientID(fmt.Sprintf("%s-s-%s", probeConfig.ClientPrefix, clientSuffix)).
		SetDefaultPublishHandler(func(client mqtt.Client, msg mqtt.Message) {
			queue <- [2]string{msg.Topic(), string(msg.Payload())}
		})

	publisher, err := connectClient(probeConfig, time.Until(setupDeadLine), publisherOptions)
	if err != nil {
		reportError("connect publish client", err)
		return
	}
	defer publisher.Disconnect(5)

	subscriber, err := connectClient(probeConfig, time.Until(setupDeadLine), subscriberOptions)
	if err != nil {
		reportError("connect subscribe client", err)
		return
	}
	defer subscriber.Disconnect(5)

	if probeConfig.SubscribeTopic == "" {
		probeConfig.SubscribeTopic = probeConfig.Topic
	}
	if token := subscriber.Subscribe(probeConfig.SubscribeTopic, qos, nil); token.WaitTimeout(time.Until(setupDeadLine)) && token.Error() != nil {
		reportError("subscribe to topic", token.Error())
		return
	}
	defer subscriber.Unsubscribe(probeConfig.SubscribeTopic)

	probeDeadline := time.Now().Add(probeTimeout)
	timeout := time.After(probeTimeout)
	receiveCount := 0

	// Support for custom message payload
	msgPayload := "This is msg %d!"
	if probeConfig.MessagePayload != "" {
		msgPayload = probeConfig.MessagePayload
	}

	for i := 0; i < num; i++ {
		text := fmt.Sprintf(msgPayload, i)
		token := publisher.Publish(probeConfig.Topic, qos, false, text)
		if !token.WaitTimeout(time.Until(probeDeadline)) {
			messagesPublishTimeout.WithLabelValues(probeConfig.Name, probeConfig.Broker).Inc()
		} else {
			messagesPublished.WithLabelValues(probeConfig.Name, probeConfig.Broker).Inc()
		}
	}

	for receiveCount < num {
		select {
		case <-queue:
			receiveCount++
			messagesReceived.WithLabelValues(probeConfig.Name, probeConfig.Broker).Inc()
		case <-timeout:
			timedoutTests.WithLabelValues(probeConfig.Name, probeConfig.Broker).Inc()
			logger.Printf("Probe %s: timed out after %d ms (received: %d)", probeConfig.Name, time.Since(t0).Milliseconds(), receiveCount)
			return
		}
	}
}

func main() {
	flag.Parse()
	yamlFile, err := os.ReadFile(*configFile)

	if err != nil {
		logger.Fatalf("Error reading config file: %s", err)
	}

	mqtt.ERROR = logger
	mqtt.CRITICAL = logger

	if *enableTrace {
		mqtt.WARN = logger
		mqtt.DEBUG = logger
	}

	cfg := config{}

	err = yaml.Unmarshal(yamlFile, &cfg)
	if err != nil {
		logger.Fatalf("Error parsing config file: %s", err)
	}

	logger.Printf("Starting mqtt_blackbox_exporter (build: %s)\n", build)

	for _, probe := range cfg.Probes {
		delay := probe.TestInterval
		if delay == 0 {
			delay = 60 * time.Second
		}
		go func(probeConfig probeConfig) {
			for {
				startProbe(&probeConfig)
				time.Sleep(delay)
			}
		}(probe)
	}

	//nolint:staticcheck
	http.Handle("/metrics", promhttp.Handler())
	err = http.ListenAndServe(*listenAddress, nil)
	if err != nil {
		logger.Fatalf("Failed to serve metrics endpoint")
	}
}
