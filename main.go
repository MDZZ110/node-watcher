package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"k8s.io/client-go/rest"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

const (
	prometheusURL    = "http://whizard-agent-proxy.kubesphere-monitoring-system.svc.cluster.local/api/v1/query"
	cpuUsageQuery    = `avg by (node, cluster) (irate(node_cpu_used_seconds_total{job="kubeedge"}[5m])) * 100`
	memoryUsageQuery = `node:node_memory_utilisation:{data_source="edge"} * 100`
	TaintName        = "edge-node-overload.edgewize.io"
)

var (
	CpuThreshold    float64
	MemoryThreshold float64
	Interval        int
)

func GetCpuUsageOverThresholdQuery() string {
	return fmt.Sprintf("%s > %f", cpuUsageQuery, CpuThreshold)
}

func GetMemoryUsageOverThresholdQuery() string {
	return fmt.Sprintf("%s > %f", memoryUsageQuery, MemoryThreshold)
}

type PrometheusResponse struct {
	Data struct {
		Result []struct {
			Metric struct {
				Node string `json:"node"`
			} `json:"metric"`
			Value []interface{} `json:"value"`
		} `json:"result"`
	} `json:"data"`
}

func main() {
	flag.Float64Var(&CpuThreshold, "cpuThreshold", 90.0, "CPU threshold to calculate CPU")
	flag.Float64Var(&MemoryThreshold, "memoryThreshold", 90.0, "Memory threshold to calculate Memory")
	flag.IntVar(&Interval, "interval", 60, "Interval in seconds")

	taintedNodeCache := make(map[string]struct{})

	// 创建Kubernetes客户端
	config, err := rest.InClusterConfig()
	if err != nil {
		log.Fatalf("Error creating in-cluster config: %v", err)
	}
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		log.Fatalf("Error creating Kubernetes client: %v", err)
	}

	ticker := time.NewTicker(time.Duration(Interval) * time.Second)
	defer ticker.Stop()

	for range ticker.C {
		// 获取超过阈值的节点
		nodesOverThreshold, err := getNodesUsage()
		if err != nil {
			log.Printf("请求监控数据报错 %v", err)
			continue
		}

		if len(nodesOverThreshold) == 0 {
			log.Printf("未发现超出阈值节点")
			continue
		}

		fmt.Printf("以下节点的 CPU 或 MEM 使用率超过 CPU 阈值 [%f] 或 MEM 阈值 [%f]: \n", CpuThreshold, MemoryThreshold)
		for nodeName, usage := range nodesOverThreshold {
			fmt.Printf("节点 %s: CPU 使用率 [%f] , MEM 使用率 [%f]\n", nodeName, usage.CPU, usage.Memory)
		}

		nodeToRemove := make([]string, len(taintedNodeCache))
		for nodeName := range taintedNodeCache {
			_, exists := nodesOverThreshold[nodeName]
			if !exists {
				nodeToRemove = append(nodeToRemove, nodeName)
			}
		}

		//// 处理每个节点的污点
		for nodeName, usage := range nodesOverThreshold {
			taintedNodeCache[nodeName] = struct{}{}
			fmt.Printf("节点 %s: CPU 使用率 [%f] , MEM 使用率 [%f]\n", nodeName, usage.CPU, usage.Memory)
			err := taintNode(clientset, nodeName)
			if err != nil {
				log.Printf("Error tainting node %s: %v", nodeName, err)
			} else {
				log.Printf("Successfully tainted node %s", nodeName)
			}
		}

		// 将没有超出阈值的节点上的污点移除
		for nodeName := range taintedNodeCache {
			err := removeTaintNode(clientset, nodeName)
			if err != nil {
				log.Printf("移除污点失败 节点 %s: %v", nodeName, err)
			} else {
				log.Printf("移除污点成功 节点 %s", nodeName)
			}
		}
	}
}

type NodeUsage struct {
	CPU    float64
	Memory float64
}

func getNodesUsage() (map[string]*NodeUsage, error) {
	cpuResults, err := queryPrometheus(GetCpuUsageOverThresholdQuery())
	if err != nil {
		return nil, err
	}

	memResults, err := queryPrometheus(GetMemoryUsageOverThresholdQuery())
	if err != nil {
		return nil, err
	}

	usageMap := make(map[string]*NodeUsage)

	// 处理CPU结果
	for _, result := range cpuResults.Data.Result {
		nodeName := normalizeNodeName(result.Metric.Node)
		if _, exists := usageMap[nodeName]; !exists {
			usageMap[nodeName] = &NodeUsage{}
		}
		if value, ok := result.Value[1].(string); ok {
			var cpuUsage float64
			fmt.Sscanf(value, "%f", &cpuUsage)
			usageMap[nodeName].CPU = cpuUsage
		}
	}

	// 处理内存结果
	for _, result := range memResults.Data.Result {
		nodeName := normalizeNodeName(result.Metric.Node)
		if _, exists := usageMap[nodeName]; !exists {
			usageMap[nodeName] = &NodeUsage{}
		}
		if value, ok := result.Value[1].(string); ok {
			var memUsage float64
			fmt.Sscanf(value, "%f", &memUsage)
			usageMap[nodeName].Memory = memUsage
		}
	}

	return usageMap, nil
}

func queryPrometheus(query string) (*PrometheusResponse, error) {
	client := &http.Client{Timeout: 10 * time.Second}
	req, err := http.NewRequest("GET", prometheusURL, nil)
	if err != nil {
		return nil, err
	}

	// 添加认证信息（根据实际环境配置）
	if token := os.Getenv("PROMETHEUS_TOKEN"); token != "" {
		req.Header.Add("Authorization", "Bearer "+token)
	}

	q := req.URL.Query()
	q.Add("query", query)
	req.URL.RawQuery = q.Encode()

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var result PrometheusResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}

	return &result, nil
}

func normalizeNodeName(instance string) string {
	// 示例转换：从 "node1.example.com" 获取 "node1"
	parts := strings.Split(instance, ".")
	if len(parts) > 0 {
		return parts[0]
	}
	return instance
}

func taintNode(clientset *kubernetes.Clientset, nodeName string) error {
	// 获取节点对象
	node, err := clientset.CoreV1().Nodes().Get(context.TODO(), nodeName, metav1.GetOptions{})
	if err != nil {
		return err
	}

	// 创建新的污点
	newTaint := v1.Taint{
		Key:    TaintName,
		Value:  time.Now().Format("2006-01-02T15:04:05"),
		Effect: v1.TaintEffectNoSchedule,
	}

	// 检查是否已存在相同key的污点
	taintExists := false
	for _, t := range node.Spec.Taints {
		if t.Key == newTaint.Key {
			taintExists = true
			break
		}
	}

	if !taintExists {
		node.Spec.Taints = append(node.Spec.Taints, newTaint)
		_, err = clientset.CoreV1().Nodes().Update(context.TODO(), node, metav1.UpdateOptions{})
		return err
	}

	return nil
}

func removeTaintNode(clientset *kubernetes.Clientset, nodeName string) error {
	// 获取节点对象
	node, err := clientset.CoreV1().Nodes().Get(context.TODO(), nodeName, metav1.GetOptions{})
	if err != nil {
		return err
	}

	newTaints := make([]v1.Taint, len(node.Spec.Taints))
	// 检查是否已存在相同key的污点
	taintExists := false
	for _, t := range node.Spec.Taints {
		if t.Key == TaintName {
			taintExists = true
			continue
		}

		newTaints = append(newTaints, t)
	}

	if taintExists {
		log.Printf("移除节点 [%s] 上污点 [%s]", nodeName, TaintName)
		node.Spec.Taints = newTaints
		_, err = clientset.CoreV1().Nodes().Update(context.TODO(), node, metav1.UpdateOptions{})
		return err
	}

	return nil
}
