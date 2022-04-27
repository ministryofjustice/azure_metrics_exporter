// I have made some very hasty modifications to the code so that it can take multiple configuration files and thus work across multiple subscriptions
// It is worth bearing in mind that I had done this trying to do the minimum amount of work and that I'm still a massive newbie with go
// Essentially there are four changes:
// 1. We now have config.file-directory to allow multiple config files to be specified easily, there is a story.
// 2. We have a map that maps config file names to a struct (prometheusConfig) that contains SafeConfig and AzureClient.
// 3. We loop through config files
//    a. In main to populate the map
//    b. In Collect, to well collect the data from the Azure API
// 4. Modified a bunch of functions so that they don't just use the global SafeConfig and AzureClient

package main

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"time"

	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"log"

	"github.com/hashicorp/logutils"

	"github.com/RobustPerception/azure_metrics_exporter/config"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/prometheus/common/version"

	kingpin "gopkg.in/alecthomas/kingpin.v2"
)

var (
	configFiles           []string
	configFilesDirectory  = kingpin.Flag("config.file-directory", "Azure exporter configuration file.").Default("./config").String()
	listenAddress         = kingpin.Flag("web.listen-address", "The address to listen on for HTTP requests.").Default(":9276").String()
	listMetricDefinitions = kingpin.Flag("list.definitions", "List available metric definitions for the given resources and exit.").Bool()
	listMetricNamespaces  = kingpin.Flag("list.namespaces", "List available metric namespaces for the given resources and exit.").Bool()
	logLevel              = kingpin.Flag("loglevel", "Log Level.").Default("DEBUG").String()
	invalidMetricChars    = regexp.MustCompile("[^a-zA-Z0-9_:]")
	azureErrorDesc        = prometheus.NewDesc("azure_error", "Error collecting metrics", nil, nil)
	batchSize             = 20
	scrapingConfig        = make(map[string]scrapeConfig)
)

func init() {
	prometheus.MustRegister(version.NewCollector("azure_exporter"))
}

// Collector generic collector type
type Collector struct{}

// Describe implemented with dummy data to satisfy interface.
func (c *Collector) Describe(ch chan<- *prometheus.Desc) {
	ch <- prometheus.NewDesc("dummy", "dummy", nil, nil)
}

type scrapeConfig struct {
	sc *config.SafeConfig
	ac *AzureClient
}
type resourceMeta struct {
	resourceID      string
	resourceURL     string
	metricNamespace string
	metrics         string
	aggregations    []string
	resource        AzureResource
}

func (c *Collector) extractMetrics(ch chan<- prometheus.Metric, rm resourceMeta, httpStatusCode int, metricValueData AzureMetricValueResponse, publishedResources map[string]bool) {

	// The Azure Monitor API seems to sometimes return the same metric multiple times, I guess depending on the sliding window, thus we store whether it's been published already.
	// This does mean that we WILL lose metrics if the same metric is published multiple times, but that's better than the alternative, which means losing all metrics on that processing run.
	// After some investigation I'm not 100% that we will lose metrics, guess we need to test properly
	processedMetrics := make(map[string]bool)

	if httpStatusCode != 200 {
		log.Printf("[WARN] Received %d status for resource %s. %s", httpStatusCode, rm.resourceURL, metricValueData.APIError.Message)
		return
	}

	if len(metricValueData.Value) == 0 || len(metricValueData.Value[0].Timeseries) == 0 {
		log.Printf("[WARN] Metric %v not found at target %v\n", rm.metrics, rm.resourceURL)
		return
	}
	if len(metricValueData.Value[0].Timeseries[0].Data) == 0 {
		log.Printf("[WARN] No metric data returned for metric %v at target %v\n", rm.metrics, rm.resourceURL)
		return
	}

	log.Print("[DEBUG] -------------- Start Extracting Metrics --------------\n")
	for _, value := range metricValueData.Value {
		// Ensure Azure metric names conform to Prometheus metric name conventions
		metricName := strings.Replace(value.Name.Value, " ", "_", -1)
		metricName = strings.ToLower(metricName + "_" + value.Unit)
		metricName = strings.Replace(metricName, "/", "_per_", -1)
		if rm.metricNamespace != "" {
			metricName = strings.ToLower(rm.metricNamespace + "_" + metricName)
		}
		metricName = invalidMetricChars.ReplaceAllString(metricName, "_")
		log.Printf("[DEBUG] Processing %s\n", metricName)

		if len(value.Timeseries) > 0 && !processedMetrics[rm.resource.ID+metricName] {
			metricValue := value.Timeseries[0].Data[len(value.Timeseries[0].Data)-1]
			labels := CreateResourceLabels(rm.resourceURL)

			if hasAggregation(rm.aggregations, "Total") {
				log.Printf("[DEBUG] Total %f\n", metricValue.Total)
				ch <- prometheus.MustNewConstMetric(
					prometheus.NewDesc(metricName+"_total", metricName+"_total", nil, labels),
					prometheus.GaugeValue,
					metricValue.Total,
				)
			}

			if hasAggregation(rm.aggregations, "Average") {
				log.Printf("[DEBUG] Total %f\n", metricValue.Average)
				ch <- prometheus.MustNewConstMetric(
					prometheus.NewDesc(metricName+"_average", metricName+"_average", nil, labels),
					prometheus.GaugeValue,
					metricValue.Average,
				)
			}

			if hasAggregation(rm.aggregations, "Minimum") {
				log.Printf("[DEBUG] Total %f\n", metricValue.Minimum)
				ch <- prometheus.MustNewConstMetric(
					prometheus.NewDesc(metricName+"_min", metricName+"_min", nil, labels),
					prometheus.GaugeValue,
					metricValue.Minimum,
				)
			}

			if hasAggregation(rm.aggregations, "Maximum") {
				log.Printf("[DEBUG] Total %f\n", metricValue.Maximum)
				ch <- prometheus.MustNewConstMetric(
					prometheus.NewDesc(metricName+"_max", metricName+"_max", nil, labels),
					prometheus.GaugeValue,
					metricValue.Maximum,
				)
			}
		} else if len(value.Timeseries) > 0 && processedMetrics[rm.resource.ID+metricName] {
			metricValue := value.Timeseries[0].Data[len(value.Timeseries[0].Data)-1]
			log.Printf("[WARN] Skipping metric %s to avoid duplicate metrics. TimeStamp: %s Total: %f Average: %f Min: %f Max: %f.\n", metricName, metricValue.TimeStamp, metricValue.Total, metricValue.Average, metricValue.Minimum, metricValue.Maximum)
		}
		processedMetrics[rm.resource.ID+metricName] = true
	}

	log.Print("[DEBUG] -------------- End Extracting Metrics--------------\n")

	if _, ok := publishedResources[rm.resource.ID]; !ok {
		infoLabels := CreateAllResourceLabelsFrom(rm)
		ch <- prometheus.MustNewConstMetric(
			prometheus.NewDesc("azure_resource_info", "Azure information available for resource", nil, infoLabels),
			prometheus.GaugeValue,
			1,
		)
		log.Printf("[DEBUG] Published: %s\n", rm.resource.ID)
		publishedResources[rm.resource.ID] = true
	}
}

func (c *Collector) batchCollectMetrics(ch chan<- prometheus.Metric, resources []resourceMeta, ac *AzureClient, sc *config.SafeConfig) {
	var publishedResources = map[string]bool{}
	// collect metrics in batches
	for i := 0; i < len(resources); i += batchSize {
		j := i + batchSize

		// don't forget to add remainder resources
		if j > len(resources) {
			j = len(resources)
		}

		var urls []string
		for _, r := range resources[i:j] {
			urls = append(urls, r.resourceURL)
		}

		batchBody, err := ac.getBatchResponseBody(urls, sc)
		if err != nil {
			ch <- prometheus.NewInvalidMetric(azureErrorDesc, err)
			return
		}

		var batchData AzureBatchMetricResponse
		err = json.Unmarshal(batchBody, &batchData)
		if err != nil {
			ch <- prometheus.NewInvalidMetric(azureErrorDesc, err)
			return
		}

		for k, resp := range batchData.Responses {
			c.extractMetrics(ch, resources[i+k], resp.HttpStatusCode, resp.Content, publishedResources)
		}
	}
}

func (c *Collector) batchLookupResources(resources []resourceMeta, ac *AzureClient, sc *config.SafeConfig) ([]resourceMeta, error) {
	var updatedResources = resources
	// collect resource info in batches
	for i := 0; i < len(resources); i += batchSize {
		j := i + batchSize
		// don't forget to add remainder resources
		if j > len(resources) {
			j = len(resources)
		}

		var urls []string
		for _, r := range resources[i:j] {
			resourceType := GetResourceType(r.resourceURL)
			if resourceType == "" {
				return nil, fmt.Errorf("No type found for resource: %s", r.resourceID)
			}

			apiVersion := ac.APIVersions.findBy(resourceType)
			if apiVersion == "" {
				return nil, fmt.Errorf("No api version found for type: %s", resourceType)
			}

			subscription := fmt.Sprintf("subscriptions/%s", sc.C.Credentials.SubscriptionID)
			resourcesEndpoint := fmt.Sprintf("/%s/%s?api-version=%s", subscription, r.resourceID, apiVersion)

			urls = append(urls, resourcesEndpoint)
		}

		batchBody, err := ac.getBatchResponseBody(urls, sc)
		if err != nil {
			return nil, err
		}

		var batchData AzureBatchLookupResponse
		err = json.Unmarshal(batchBody, &batchData)
		if err != nil {
			return nil, fmt.Errorf("Error unmarshalling response body: %v", err)
		}

		for k, resp := range batchData.Responses {
			updatedResources[i+k].resource = resp.Content
			updatedResources[i+k].resource.Subscription = sc.C.Credentials.SubscriptionID
		}
	}
	return updatedResources, nil
}

// Collect - collect results from Azure Montior API and create Prometheus metrics.
func (c *Collector) Collect(ch chan<- prometheus.Metric) {
	//This is 100% not a copy and paste from main, I swear
	for _, configFile := range configFiles {
		start := time.Now()
		if err := scrapingConfig[configFile].ac.refreshAccessToken(scrapingConfig[configFile].sc); err != nil {
			log.Println(err)
			ch <- prometheus.NewInvalidMetric(azureErrorDesc, err)
			return
		}

		var resources []resourceMeta
		var incompleteResources []resourceMeta

		for _, target := range scrapingConfig[configFile].sc.C.Targets {
			var rm resourceMeta

			metrics := []string{}
			for _, metric := range target.Metrics {
				metrics = append(metrics, metric.Name)
			}

			rm.resourceID = target.Resource
			rm.metricNamespace = target.MetricNamespace
			rm.metrics = strings.Join(metrics, ",")
			rm.aggregations = filterAggregations(target.Aggregations)
			rm.resourceURL = resourceURLFrom(target.Resource, rm.metricNamespace, rm.metrics, rm.aggregations, scrapingConfig[configFile].sc)
			incompleteResources = append(incompleteResources, rm)
		}

		for _, resourceGroup := range scrapingConfig[configFile].sc.C.ResourceGroups {
			metrics := []string{}
			for _, metric := range resourceGroup.Metrics {
				metrics = append(metrics, metric.Name)
			}
			metricsStr := strings.Join(metrics, ",")

			filteredResources, err := scrapingConfig[configFile].ac.filteredListFromResourceGroup(resourceGroup, scrapingConfig[configFile].sc)
			if err != nil {
				log.Printf("[ERROR] Failed to get resources for resource group %s and resource types %s: %v",
					resourceGroup.ResourceGroup, resourceGroup.ResourceTypes, err)
				ch <- prometheus.NewInvalidMetric(azureErrorDesc, err)
				return
			}

			for _, f := range filteredResources {
				var rm resourceMeta
				rm.resourceID = f.ID
				rm.metricNamespace = resourceGroup.MetricNamespace
				rm.metrics = metricsStr
				rm.aggregations = filterAggregations(resourceGroup.Aggregations)
				rm.resourceURL = resourceURLFrom(f.ID, rm.metricNamespace, rm.metrics, rm.aggregations, scrapingConfig[configFile].sc)
				rm.resource = f
				resources = append(resources, rm)
			}
		}

		resourcesCache := make(map[string][]byte)
		for _, resourceTag := range scrapingConfig[configFile].sc.C.ResourceTags {
			metrics := []string{}
			for _, metric := range resourceTag.Metrics {
				metrics = append(metrics, metric.Name)
			}
			metricsStr := strings.Join(metrics, ",")

			filteredResources, err := scrapingConfig[configFile].ac.filteredListByTag(resourceTag, resourcesCache, scrapingConfig[configFile].sc)
			if err != nil {
				log.Printf("[ERROR] Failed to get resources for tag name %s, tag value %s: %v",
					resourceTag.ResourceTagName, resourceTag.ResourceTagValue, err)
				ch <- prometheus.NewInvalidMetric(azureErrorDesc, err)
				return
			}

			for _, f := range filteredResources {
				var rm resourceMeta
				rm.resourceID = f.ID
				rm.metricNamespace = resourceTag.MetricNamespace
				rm.metrics = metricsStr
				rm.aggregations = filterAggregations(resourceTag.Aggregations)
				rm.resourceURL = resourceURLFrom(f.ID, rm.metricNamespace, rm.metrics, rm.aggregations, scrapingConfig[configFile].sc)
				incompleteResources = append(incompleteResources, rm)
			}
		}

		completeResources, err := c.batchLookupResources(incompleteResources, scrapingConfig[configFile].ac, scrapingConfig[configFile].sc)
		if err != nil {
			log.Printf("[ERROR] Failed to get resource info: %s", err)
			ch <- prometheus.NewInvalidMetric(azureErrorDesc, err)
			return
		}

		resources = append(resources, completeResources...)
		c.batchCollectMetrics(ch, resources, scrapingConfig[configFile].ac, scrapingConfig[configFile].sc)
		duration := time.Since(start)
		log.Printf("[DEBUG] Processed %s in %s. ", configFile, duration)
	}
}

func handler(w http.ResponseWriter, r *http.Request) {
	registry := prometheus.NewRegistry()
	collector := &Collector{}
	registry.MustRegister(collector)
	h := promhttp.HandlerFor(registry, promhttp.HandlerOpts{})
	h.ServeHTTP(w, r)
}

//This is used to find the config files
func findFiles(root, ext string) []string {
	var files []string
	filepath.WalkDir(root, func(fileName string, dirEntry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if filepath.Ext(dirEntry.Name()) == ext {
			files = append(files, fileName)
		}
		return nil
	})
	return files
}

func main() {
	kingpin.HelpFlag.Short('h')
	kingpin.Parse()

	filter := &logutils.LevelFilter{
		Levels:   []logutils.LogLevel{"DEBUG", "WARN", "ERROR"},
		MinLevel: logutils.LogLevel(*logLevel),
		Writer:   os.Stderr,
	}
	log.SetOutput(filter)

	//I was wondering how difficult it would be to make this take several config files
	configFiles = findFiles(*configFilesDirectory, ".yml")
	for _, configFile := range configFiles {
		scrapingConfig[configFile] = scrapeConfig{ac: NewAzureClient(),
			sc: &config.SafeConfig{
				C: &config.Config{},
			}}

		if err := scrapingConfig[configFile].sc.ReloadConfig(configFile); err != nil {
			log.Fatalf("Error loading config: %v", err)
		}

		err := scrapingConfig[configFile].ac.getAccessToken(scrapingConfig[configFile].sc)
		if err != nil {
			log.Fatalf("Failed to get token: %v", err)
		}

		// Print list of available metric definitions for each resource to console if specified.
		if *listMetricDefinitions {
			results, err := scrapingConfig[configFile].ac.getMetricDefinitions(scrapingConfig[configFile].sc)
			if err != nil {
				log.Fatalf("Failed to fetch metric definitions: %v", err)
			}

			for k, v := range results {
				log.Printf("Resource: %s\n\nAvailable Metrics:\n", k)
				for _, r := range v.MetricDefinitionResponses {
					log.Printf("- %s\n", r.Name.Value)
				}
			}
			os.Exit(0)
		}

		// Print list of available metric namespace for each resource to console if specified.
		if *listMetricNamespaces {
			results, err := scrapingConfig[configFile].ac.getMetricNamespaces(scrapingConfig[configFile].sc)
			if err != nil {
				log.Fatalf("Failed to fetch metric namespaces: %v", err)
			}

			for k, v := range results {
				log.Printf("Resource: %s\n\nAvailable namespaces:\n", k)
				for _, namespace := range v.MetricNamespaceCollection {
					log.Printf("- %s\n", namespace.Properties.MetricNamespaceName)
				}
			}
			os.Exit(0)
		}

		err = scrapingConfig[configFile].ac.listAPIVersions(scrapingConfig[configFile].sc)
		if err != nil {
			log.Fatal(err)
		}
	}

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`<html>
            <head>
            <title>Azure Exporter</title>
            </head>
            <body>
            <h1>Azure Exporter</h1>
						<p><a href="/metrics">Metrics</a></p>
            </body>
            </html>`))
	})

	http.HandleFunc("/metrics", handler)
	log.Printf("azure_metrics_exporter listening on port %v", *listenAddress)
	if err := http.ListenAndServe(*listenAddress, nil); err != nil {
		log.Fatalf("Error starting HTTP server: %v", err)
	}
}
