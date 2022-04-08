// I have made some very hasty modifications to the code so that it can take multiple configuration files and thus work across multiple subscriptions
// It is worth bearing in mind that I had done this trying to do the minimum amount of work and that I'm still a massive newbie with go
// Essentially the change is three fold:
// 1. SafeConfig  and AzureClient are now Arrays
// 2. We loop through config files
//    a. In main to populate SafeConfig and AzureClient arrays
//    b. In Collect we loop through the AzureClients
// 3. Modify a bunch of functions so that they don't just use the global SafeConfig and AzureClient

package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"regexp"
	"strings"

	"github.com/RobustPerception/azure_metrics_exporter/config"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/prometheus/common/version"

	kingpin "gopkg.in/alecthomas/kingpin.v2"
)

var (
	//why 42? why not? I know this is less than ideal but time constraints and all that.
	sc                    [42]*config.SafeConfig
	ac                    [42]*AzureClient
	configFiles           = kingpin.Flag("config.file", "Azure exporter configuration file.").Default("azure.yml", "azure2.yml").Strings()
	listenAddress         = kingpin.Flag("web.listen-address", "The address to listen on for HTTP requests.").Default(":9276").String()
	listMetricDefinitions = kingpin.Flag("list.definitions", "List available metric definitions for the given resources and exit.").Bool()
	listMetricNamespaces  = kingpin.Flag("list.namespaces", "List available metric namespaces for the given resources and exit.").Bool()
	invalidMetricChars    = regexp.MustCompile("[^a-zA-Z0-9_:]")
	azureErrorDesc        = prometheus.NewDesc("azure_error", "Error collecting metrics", nil, nil)
	batchSize             = 20
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

type resourceMeta struct {
	resourceID      string
	resourceURL     string
	metricNamespace string
	metrics         string
	aggregations    []string
	resource        AzureResource
}

func (c *Collector) extractMetrics(ch chan<- prometheus.Metric, rm resourceMeta, httpStatusCode int, metricValueData AzureMetricValueResponse, publishedResources map[string]bool) {
	if httpStatusCode != 200 {
		log.Printf("Received %d status for resource %s. %s", httpStatusCode, rm.resourceURL, metricValueData.APIError.Message)
		return
	}

	if len(metricValueData.Value) == 0 || len(metricValueData.Value[0].Timeseries) == 0 {
		log.Printf("Metric %v not found at target %v\n", rm.metrics, rm.resourceURL)
		return
	}
	if len(metricValueData.Value[0].Timeseries[0].Data) == 0 {
		log.Printf("No metric data returned for metric %v at target %v\n", rm.metrics, rm.resourceURL)
		return
	}

	for _, value := range metricValueData.Value {
		// Ensure Azure metric names conform to Prometheus metric name conventions
		metricName := strings.Replace(value.Name.Value, " ", "_", -1)
		metricName = strings.ToLower(metricName + "_" + value.Unit)
		metricName = strings.Replace(metricName, "/", "_per_", -1)
		if rm.metricNamespace != "" {
			metricName = strings.ToLower(rm.metricNamespace + "_" + metricName)
		}
		metricName = invalidMetricChars.ReplaceAllString(metricName, "_")

		if len(value.Timeseries) > 0 {
			metricValue := value.Timeseries[0].Data[len(value.Timeseries[0].Data)-1]
			labels := CreateResourceLabels(rm.resourceURL)

			if hasAggregation(rm.aggregations, "Total") {
				ch <- prometheus.MustNewConstMetric(
					prometheus.NewDesc(metricName+"_total", metricName+"_total", nil, labels),
					prometheus.GaugeValue,
					metricValue.Total,
				)
			}

			if hasAggregation(rm.aggregations, "Average") {
				ch <- prometheus.MustNewConstMetric(
					prometheus.NewDesc(metricName+"_average", metricName+"_average", nil, labels),
					prometheus.GaugeValue,
					metricValue.Average,
				)
			}

			if hasAggregation(rm.aggregations, "Minimum") {
				ch <- prometheus.MustNewConstMetric(
					prometheus.NewDesc(metricName+"_min", metricName+"_min", nil, labels),
					prometheus.GaugeValue,
					metricValue.Minimum,
				)
			}

			if hasAggregation(rm.aggregations, "Maximum") {
				ch <- prometheus.MustNewConstMetric(
					prometheus.NewDesc(metricName+"_max", metricName+"_max", nil, labels),
					prometheus.GaugeValue,
					metricValue.Maximum,
				)
			}
		}
	}

	if _, ok := publishedResources[rm.resource.ID]; !ok {
		infoLabels := CreateAllResourceLabelsFrom(rm)
		ch <- prometheus.MustNewConstMetric(
			prometheus.NewDesc("azure_resource_info", "Azure information available for resource", nil, infoLabels),
			prometheus.GaugeValue,
			1,
		)
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
	for i, _ := range *configFiles {
		if err := ac[i].refreshAccessToken(sc[i]); err != nil {
			log.Println(err)
			ch <- prometheus.NewInvalidMetric(azureErrorDesc, err)
			return
		}

		var resources []resourceMeta
		var incompleteResources []resourceMeta

		for _, target := range sc[i].C.Targets {
			var rm resourceMeta

			metrics := []string{}
			for _, metric := range target.Metrics {
				metrics = append(metrics, metric.Name)
			}

			rm.resourceID = target.Resource
			rm.metricNamespace = target.MetricNamespace
			rm.metrics = strings.Join(metrics, ",")
			rm.aggregations = filterAggregations(target.Aggregations)
			rm.resourceURL = resourceURLFrom(target.Resource, rm.metricNamespace, rm.metrics, rm.aggregations, sc[i])
			incompleteResources = append(incompleteResources, rm)
		}

		for _, resourceGroup := range sc[i].C.ResourceGroups {
			metrics := []string{}
			for _, metric := range resourceGroup.Metrics {
				metrics = append(metrics, metric.Name)
			}
			metricsStr := strings.Join(metrics, ",")

			filteredResources, err := ac[i].filteredListFromResourceGroup(resourceGroup, sc[i])
			if err != nil {
				log.Printf("Failed to get resources for resource group %s and resource types %s: %v",
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
				rm.resourceURL = resourceURLFrom(f.ID, rm.metricNamespace, rm.metrics, rm.aggregations, sc[i])
				rm.resource = f
				resources = append(resources, rm)
			}
		}

		resourcesCache := make(map[string][]byte)
		for _, resourceTag := range sc[i].C.ResourceTags {
			metrics := []string{}
			for _, metric := range resourceTag.Metrics {
				metrics = append(metrics, metric.Name)
			}
			metricsStr := strings.Join(metrics, ",")

			filteredResources, err := ac[i].filteredListByTag(resourceTag, resourcesCache, sc[i])
			if err != nil {
				log.Printf("Failed to get resources for tag name %s, tag value %s: %v",
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
				rm.resourceURL = resourceURLFrom(f.ID, rm.metricNamespace, rm.metrics, rm.aggregations, sc[i])
				incompleteResources = append(incompleteResources, rm)
			}
		}

		completeResources, err := c.batchLookupResources(incompleteResources, ac[i], sc[i])
		if err != nil {
			log.Printf("Failed to get resource info: %s", err)
			ch <- prometheus.NewInvalidMetric(azureErrorDesc, err)
			return
		}

		resources = append(resources, completeResources...)
		c.batchCollectMetrics(ch, resources, ac[i], sc[i])
	}
}

func handler(w http.ResponseWriter, r *http.Request) {
	registry := prometheus.NewRegistry()
	collector := &Collector{}
	registry.MustRegister(collector)
	h := promhttp.HandlerFor(registry, promhttp.HandlerOpts{})
	h.ServeHTTP(w, r)
}

func main() {
	kingpin.HelpFlag.Short('h')
	kingpin.Parse()
	//I was wondering how difficult it would be to make this take several config files
	for i, configFile := range *configFiles {
		ac[i] = NewAzureClient()
		sc[i] = &config.SafeConfig{
			C: &config.Config{},
		}

		if err := sc[i].ReloadConfig(configFile); err != nil {
			log.Fatalf("Error loading config: %v", err)
		}

		err := ac[i].getAccessToken(sc[i])
		if err != nil {
			log.Fatalf("Failed to get token: %v", err)
		}

		// Print list of available metric definitions for each resource to console if specified.
		if *listMetricDefinitions {
			results, err := ac[i].getMetricDefinitions(sc[i])
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
			results, err := ac[i].getMetricNamespaces(sc[i])
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

		err = ac[i].listAPIVersions(sc[i])
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
