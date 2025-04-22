package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
	"gopkg.in/yaml.v2"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

type Controller struct {
	Clientset *kubernetes.Clientset
	Namespace string
}

type CmdConfig struct {
	ConfigMapName          string
	ConfigMapLimitsPath    string
	ConfigMapGeneratedName string
	ReceiverLabel          string
	ActiveSeriesMax        int
	Interval               time.Duration
}

// https://thanos.io/tip/components/receive.md/#understanding-the-configuration-file
// Take an existing configmap as an input, and override (for now) the `write.global.samples_limit`
type WriteConfig struct {
	Write LimitsConfig `yaml:"write"`
}

type LimitsConfig struct {
	Global  GlobalConfig             `yaml:"global"`
	Default TenantConfig             `yaml:"default"`
	Tenant  map[string]*TenantConfig `yaml:"tenants,omitempty"`
}

type GlobalConfig struct {
	MaxConcurrency           *int   `yaml:"max_concurrency,omitempty"`
	MetaMonitoringURL        string `yaml:"meta_monitoring_url"`
	MetaMonitoringLimitQuery string `yaml:"meta_monitoring_limit_query"`
}

type TenantConfig struct {
	Request         *RequestConfig `yaml:"request,omitempty"`
	HeadSeriesLimit *int           `yaml:"head_series_limit,omitempty"`
}

type RequestConfig struct {
	SizeBytesLimit *int `yaml:"size_bytes_limit,omitempty"`
	SeriesLimit    *int `yaml:"series_limit,omitempty"`
	SamplesLimit   *int `yaml:"samples_limit,omitempty"`
}

func parseFlags() CmdConfig {
	var config CmdConfig

	flag.StringVar(&config.ConfigMapName, "configmap-name", "", "The previous limits configuration configmap containing the limits configuration.")
	flag.StringVar(&config.ConfigMapLimitsPath, "configmap-limits-path", "config.yaml", "The default location of the limits configuration within the ConfigMap.")
	flag.StringVar(&config.ConfigMapGeneratedName, "configmap-generated-name", "", "The name given to the configmap containing the limits configuration.")
	flag.StringVar(&config.ReceiverLabel, "statefulset-label", "controller.limits.thanos.io=thanos-limits-controller", "The statefulset's label to watch by the controller.")
	flag.IntVar(&config.ActiveSeriesMax, "active-series-max", 0, "The maximum the number that a single particular receive instance can handle.")
	flag.DurationVar(&config.Interval, "interval", 0, "Optional interval for periodic reconciliation (e.g. 30s, 1m). If 0, runs once and exits.")
	flag.Parse()

	return config
}

func (c CmdConfig) validate() error {
	if c.ConfigMapName == "" {
		return fmt.Errorf("missing required flag: -configmap-name")
	}
	if c.ActiveSeriesMax == 0 {
		return fmt.Errorf("missing or invalid flag: -active-series-max")
	}
	if c.ConfigMapGeneratedName == "" {
		return fmt.Errorf("missing required flag: -configmap-generated-name")
	}
	return nil
}

func init() {
	logLevelStr := os.Getenv("LOG_LEVEL")

	if logLevelStr == "" {
		log.SetLevel(log.InfoLevel)
		return
	}

	logLevelStr = strings.ToLower(logLevelStr)

	switch logLevelStr {
	case "debug":
		log.SetLevel(log.DebugLevel)
	case "info":
		log.SetLevel(log.InfoLevel)
	case "warn", "warning":
		log.SetLevel(log.WarnLevel)
	case "error":
		log.SetLevel(log.ErrorLevel)
	case "fatal":
		log.SetLevel(log.FatalLevel)
	case "panic":
		log.SetLevel(log.PanicLevel)
	case "trace":
		log.SetLevel(log.TraceLevel)
	default:
		log.SetLevel(log.InfoLevel)
	}
}

func main() {
	cmdConfig := parseFlags()

	labelSelector := cmdConfig.ReceiverLabel

	if err := cmdConfig.validate(); err != nil {
		log.Fatal(err)
	}

	execute := func() {
		controller, err := NewController()
		if err != nil {
			log.Fatalf("Failed to initialize controller: %v", err)
		}

		runningReplicas := controller.getRunningStatefulSets(labelSelector)
		globalLimit := runningReplicas * cmdConfig.ActiveSeriesMax
		log.Debugf("Calculated global head_series_limit: %d", globalLimit)

		limitsConfig, err := controller.getLimitsConfigMap(cmdConfig.ConfigMapName, cmdConfig.ConfigMapLimitsPath)
		if err != nil {
			log.Fatalf("Error fetching the configmap %s, %v", cmdConfig.ConfigMapName, err)
		}

		err = controller.createGeneratedConfigMap(cmdConfig.ConfigMapGeneratedName, cmdConfig.ConfigMapLimitsPath, limitsConfig, globalLimit)
		if err != nil {
			log.Fatalf("Failed to create or update configmap: %v", err)
		}
	}

	if cmdConfig.Interval > 0 {
		ticker := time.NewTicker(cmdConfig.Interval)
		defer ticker.Stop()
		for {
			execute()
			<-ticker.C
		}
	} else {
		execute()
	}
}

// getKubernetesClient creates a Kubernetes clientset
func getKubernetesClient() (*kubernetes.Clientset, error) {
	var config *rest.Config
	var err error

	// Try in-cluster config first
	config, err = rest.InClusterConfig()
	if err != nil {
		// Fall back to kubeconfig
		log.Debug("Not running in cluster, using kubeconfig")
		kubeconfig := filepath.Join(os.Getenv("HOME"), ".kube", "config")
		config, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
		if err != nil {
			return nil, fmt.Errorf("error building kubeconfig: %v", err)
		}
	}

	return kubernetes.NewForConfig(config)
}

func getCurrentNamespace() (string, error) {
	// Try to get namespace from service account
	data, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/namespace")
	if err == nil {
		return string(data), nil
	}

	// If not running in a pod, check if NAMESPACE env var is set
	namespace := os.Getenv("NAMESPACE")
	if namespace != "" {
		return namespace, nil
	}

	// Otherwise, use the current context's namespace from kubeconfig
	kubeconfig := filepath.Join(os.Getenv("HOME"), ".kube", "config")
	config, err := clientcmd.LoadFromFile(kubeconfig)
	if err != nil {
		return "default", nil // Default to "default" namespace as last resort
	}

	context := config.Contexts[config.CurrentContext]
	if context != nil && context.Namespace != "" {
		return context.Namespace, nil
	}

	return "default", nil
}

// getRunningStatefulSets returns the number of ready replicas for statefulsets matching a label.
func (c *Controller) getRunningStatefulSets(labelSelector string) int {
	// List StatefulSets with the given label
	statefulSets, err := c.Clientset.AppsV1().StatefulSets(c.Namespace).List(context.TODO(), metav1.ListOptions{
		LabelSelector: labelSelector,
	})
	if err != nil {
		log.Fatalf("error listing StatefulSets: %v", err)
	}

	// Then filter for only those in running state (where ReadyReplicas equals Replicas)
	var runningReplicas int32
	for _, sts := range statefulSets.Items {
		runningReplicas += sts.Status.ReadyReplicas
		log.Debugf("StatefulSet %s is running with %d/%d ready replicas",
			sts.Name, sts.Status.ReadyReplicas, sts.Status.Replicas)
	}

	log.Debugf("Returned running replicas are: %d", runningReplicas)
	return int(runningReplicas)
}

func (c *Controller) getLimitsConfigMap(configMapName string, configMapPath string) (*WriteConfig, error) {

	configMapData, err := c.Clientset.CoreV1().ConfigMaps(c.Namespace).Get(context.TODO(), configMapName, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("Error locating ConfigMap: %v", err)
	}

	limitsConfig, exists := configMapData.Data[configMapPath]
	if !exists {
		return nil, fmt.Errorf("key %s not fond in ConfigMap %s", configMapPath, configMapName)
	}

	var parsedConfig WriteConfig
	unmarshalErr := yaml.Unmarshal([]byte(limitsConfig), &parsedConfig)
	if unmarshalErr != nil {
		return nil, fmt.Errorf("failed to parse limits config: %w", unmarshalErr)
	}

	return &parsedConfig, nil
}

func (c *Controller) createGeneratedConfigMap(configMapGeneratedName string, configMapPath string, config *WriteConfig, headSeriesValue int) error {

	config.Write.Default.HeadSeriesLimit = &headSeriesValue

	updatedYAML, err := yaml.Marshal(config)
	if err != nil {
		return fmt.Errorf("failed to marshal updated config: %w", err)
	}

	// Create the new ConfigMap object
	newConfigMap := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name: configMapGeneratedName,
		},
		Data: map[string]string{
			configMapPath: string(updatedYAML),
		},
	}

	// Attempt to create the ConfigMap
	_, err = c.Clientset.CoreV1().ConfigMaps(c.Namespace).Create(context.TODO(), newConfigMap, metav1.CreateOptions{})
	if err == nil {
		log.Infof("Successfully created ConfigMap: %s", configMapGeneratedName)
		return nil
	}

	// If it already exists, update it
	if strings.Contains(err.Error(), "already exists") {
		log.Infof("ConfigMap %s already exists. Updating...", configMapGeneratedName)

		// Retrieve existing ConfigMap to get its resource version
		existing, getErr := c.Clientset.CoreV1().ConfigMaps(c.Namespace).Get(context.TODO(), configMapGeneratedName, metav1.GetOptions{})
		if getErr != nil {
			return fmt.Errorf("failed to fetch existing ConfigMap for update: %w", getErr)
		}

		// Set the resource version to allow update
		newConfigMap.ResourceVersion = existing.ResourceVersion

		_, updateErr := c.Clientset.CoreV1().ConfigMaps(c.Namespace).Update(context.TODO(), newConfigMap, metav1.UpdateOptions{})
		if updateErr != nil {
			return fmt.Errorf("failed to update existing ConfigMap: %w", updateErr)
		}

		log.Infof("Successfully updated ConfigMap: %s", configMapGeneratedName)
		return nil
	}

	return nil
}

func NewController() (*Controller, error) {
	clientset, err := getKubernetesClient()
	if err != nil {
		return nil, err
	}
	namespace, err := getCurrentNamespace()
	if err != nil {
		return nil, err
	}
	return &Controller{Clientset: clientset, Namespace: namespace}, nil
}
