package main

import (
	"context"
	"encoding/csv"
	"flag"
	"fmt"
	"github.com/Dreamacro/clash/adapter"
	"github.com/Dreamacro/clash/adapter/provider"
	C "github.com/Dreamacro/clash/constant"
	"github.com/Dreamacro/clash/log"
	"gopkg.in/yaml.v3"
	"io"
	"net"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

var (
	livenessObject       = flag.String("l", "https://speed.cloudflare.com/__down?bytes=%d", "liveness object, support http(s) url, support payload too")
	configPathConfig     = flag.String("c", "", "configuration file path, also support http(s) url")
	filterRegexConfig    = flag.String("f", ".*", "filter proxies that need to speedtest, use regexp")
	negFilterRegexConfig = flag.String("nf", "", "filter proxies that skip speedtest, use regexp")
	downloadSizeConfig   = flag.Int("size", 100, "download size for testing proxies(Mb)")
	timeoutConfig        = flag.Int("timeout", 5, "timeout for testing proxies")
	sortField            = flag.String("sort", "b", "sort field for testing proxies, b for bandwidth, t for TTFB")
	output               = flag.String("output", "", "output result to csv/yaml file")
	concurrent           = flag.Int("concurrent", 4, "download concurrent size")
	isFilterUsed         = flag.Bool("flt", false, "if use filter to remove low-quality proxies")
	maxLatency           = flag.Float64("lt", 2000, "max latency(ms)")
	minBandwidth         = flag.Float64("bdwd", 2, "min bandwidth(Mbps)")
	fileName             = flag.String("fn", "proxies_filtered.yaml", "output result to csv/yaml file")
)

type CProxy struct {
	C.Proxy
	SecretConfig any
}

type Result struct {
	Name      string
	Bandwidth float64
	TTFB      time.Duration
}

var (
	red   = "\033[31m"
	green = "\033[32m"
)

type RawConfig struct {
	Providers map[string]map[string]any `yaml:"proxy-providers"`
	Proxies   []map[string]any          `yaml:"proxies"`
}

func main() {
	flag.Parse()

	timeoutConfig := time.Duration(*timeoutConfig) * time.Second
	downloadSizeConfig := *downloadSizeConfig * 1024 * 1024

	C.UA = "clash.meta"

	if *configPathConfig == "" {
		log.Fatalln("Please specify the configuration file")
	}

	var allProxies = make(map[string]CProxy)
	for _, configPath := range strings.Split(*configPathConfig, ",") {
		var body []byte
		var err error
		if strings.HasPrefix(configPath, "http") {
			var resp *http.Response
			resp, err = http.Get(configPath)
			if err != nil {
				log.Warnln("failed to fetch config: %s", err)
				continue
			}
			body, err = io.ReadAll(resp.Body)
		} else {
			body, err = os.ReadFile(configPath)
		}
		if err != nil {
			log.Warnln("failed to read config: %s", err)
			continue
		}

		lps, err := loadProxies(body)
		if err != nil {
			log.Fatalln("Failed to convert : %s", err)
		}

		for k, p := range lps {
			if _, ok := allProxies[k]; !ok {
				allProxies[k] = p
			}
		}
	}

	filteredProxies := filterProxies(*filterRegexConfig, *negFilterRegexConfig, allProxies)
	results := make([]Result, 0, len(filteredProxies))

	format := "%s%-42s\t%-12s\t%-12s\033[0m\n"

	fmt.Printf(format, "", "节点", "带宽", "延迟")
	for _, name := range filteredProxies {
		proxy := allProxies[name]
		switch proxy.Type() {
		case C.Shadowsocks, C.ShadowsocksR, C.Snell, C.Socks5, C.Http, C.Vmess, C.Vless, C.Trojan, C.Hysteria, C.Hysteria2, C.WireGuard, C.Tuic:
			result := TestProxyConcurrent(name, proxy, downloadSizeConfig, timeoutConfig, *concurrent)
			result.Printf(format)
			results = append(results, *result)
		case C.Direct, C.Reject, C.Relay, C.Selector, C.Fallback, C.URLTest, C.LoadBalance:
			continue
		default:
			log.Fatalln("Unsupported proxy type: %s", proxy.Type())
		}
	}

	if *sortField != "" {
		switch *sortField {
		case "b", "bandwidth":
			sort.Slice(results, func(i, j int) bool {
				return results[i].Bandwidth > results[j].Bandwidth
			})
			fmt.Println("\n\n===结果按照带宽排序===")
		case "t", "ttfb":
			sort.Slice(results, func(i, j int) bool {
				return results[i].TTFB < results[j].TTFB
			})
			fmt.Println("\n\n===结果按照延迟排序===")
		default:
			log.Fatalln("Unsupported sort field: %s", *sortField)
		}
		fmt.Printf(format, "", "节点", "带宽", "延迟")
		for _, result := range results {
			result.Printf(format)
		}
	}

	if strings.EqualFold(*output, "yaml") && !*isFilterUsed {
		if err := writeNodeConfigurationToYAML(*fileName, results, allProxies); err != nil {
			log.Fatalln("Failed to write yaml: %s", err)
		}
	} else if strings.EqualFold(*output, "csv") {
		if err := writeToCSV(*fileName, results); err != nil {
			log.Fatalln("Failed to write csv: %s", err)
		}
	} else if strings.EqualFold(*output, "yaml") && *isFilterUsed {
		if err := writeNodeConfigurationToYAMLFiltered(*fileName, results, allProxies, *minBandwidth, *maxLatency); err != nil {
			log.Fatalln("Failed to write yaml with info: %s", err)
		}
	}

}

func writeNodeConfigurationToYAMLFiltered(filePath string, results []Result, proxies map[string]CProxy,
	minBandwidth float64, maxLatency float64) error {
	fp, err := os.Create(filePath)
	if err != nil {
		return err
	}
	defer func(fp *os.File) {
		err := fp.Close()
		if err != nil {

		}
	}(fp)

	var sortedProxies []any
	for _, result := range results {
		if v, ok := proxies[result.Name]; ok {
			if result.Bandwidth > minBandwidth*1024*1024 && (float64(result.TTFB.Milliseconds()) < maxLatency &&
				float64(result.TTFB.Milliseconds()) > 0) {
				if configMap, ok := v.SecretConfig.(map[string]any); ok {
					if _, ok := configMap["name"].(string); ok {
						configMap["name"] = fmt.Sprintf("%s%s", configMap["name"], formatBandwidthSuffix(result.Bandwidth))
						sortedProxies = append(sortedProxies, configMap)
					}
				}
			}
		}
	}

	for name, proxy := range proxies {
		if !contains(results, name) {
			sortedProxies = append(sortedProxies, proxy.SecretConfig)
		}
	}

	bytes, err := yaml.Marshal(map[string]any{"proxies": sortedProxies})

	if err != nil {
		return err
	}

	_, err = fp.Write(bytes)
	return err
}

func contains(results []Result, name string) bool {
	for _, result := range results {
		if result.Name == name {
			return true
		}
	}
	return false
}

// 辅助函数，用于格式化带宽值
func formatBandwidthSuffix(bandwidth float64) string {
	const (
		Mbps = 1024 * 1024
		Gbps = Mbps * 1024
	)
	var suffix string
	switch {
	case bandwidth >= Gbps:
		suffix = fmt.Sprintf("-%dGBPS", int(bandwidth/Gbps))
	case bandwidth >= Mbps:
		suffix = fmt.Sprintf("-%dMBPS", int(bandwidth/Mbps))
	default:
		suffix = "-0MBPS"
	}
	return suffix
}

func filterProxies(filter string, negFilter string, proxies map[string]CProxy) []string {
	filterRegexp := regexp.MustCompile(filter)
	var negFilterRegexp *regexp.Regexp
	if negFilter != "" {
		negFilterRegexp = regexp.MustCompile(negFilter)
	}
	filteredProxies := make([]string, 0, len(proxies))

	for name := range proxies {
		if filterRegexp.MatchString(name) && (negFilterRegexp == nil || !negFilterRegexp.MatchString(name)) {
			filteredProxies = append(filteredProxies, name)
		}
	}

	sort.Strings(filteredProxies)
	return filteredProxies
}

func loadProxies(buf []byte) (map[string]CProxy, error) {
	rawCfg := &RawConfig{
		Proxies: []map[string]any{},
	}
	if err := yaml.Unmarshal(buf, rawCfg); err != nil {
		return nil, err
	}
	proxies := make(map[string]CProxy)
	proxiesConfig := rawCfg.Proxies
	providersConfig := rawCfg.Providers

	for i, config := range proxiesConfig {
		proxy, err := adapter.ParseProxy(config)
		if err != nil {
			return nil, fmt.Errorf("proxy %d: %w", i, err)
		}

		if _, exist := proxies[proxy.Name()]; exist {
			return nil, fmt.Errorf("proxy %s is the duplicate name", proxy.Name())
		}
		proxies[proxy.Name()] = CProxy{Proxy: proxy, SecretConfig: config}
	}
	for name, config := range providersConfig {
		if name == provider.ReservedName {
			return nil, fmt.Errorf("can not defined a provider called `%s`", provider.ReservedName)
		}
		pd, err := provider.ParseProxyProvider(name, config)
		if err != nil {
			return nil, fmt.Errorf("parse proxy provider %s error: %w", name, err)
		}
		if err := pd.Initial(); err != nil {
			return nil, fmt.Errorf("initial proxy provider %s error: %w", pd.Name(), err)
		}
		for _, proxy := range pd.Proxies() {
			proxies[fmt.Sprintf("[%s] %s", name, proxy.Name())] = CProxy{Proxy: proxy}
		}
	}
	return proxies, nil
}

func (r *Result) Printf(format string) {
	color := ""
	if r.Bandwidth < 1024*1024 {
		color = red
	} else if r.Bandwidth > 1024*1024*10 {
		color = green
	}
	fmt.Printf(format, color, formatName(r.Name), formatBandwidth(r.Bandwidth), formatMilliseconds(r.TTFB))
}

func TestProxyConcurrent(name string, proxy C.Proxy, downloadSize int, timeout time.Duration, concurrentCount int) *Result {
	if concurrentCount <= 0 {
		concurrentCount = 1
	}

	chunkSize := downloadSize / concurrentCount
	totalTTFB := int64(0)
	downloaded := int64(0)

	var wg sync.WaitGroup
	start := time.Now()
	for i := 0; i < concurrentCount; i++ {
		wg.Add(1)
		go func(i int) {
			result, w := TestProxy(name, proxy, chunkSize, timeout)
			if w != 0 {
				atomic.AddInt64(&downloaded, w)
				atomic.AddInt64(&totalTTFB, int64(result.TTFB))
			}
			wg.Done()
		}(i)
	}
	wg.Wait()
	downloadTime := time.Since(start)

	result := &Result{
		Name:      name,
		Bandwidth: float64(downloaded) / downloadTime.Seconds(),
		TTFB:      time.Duration(totalTTFB / int64(concurrentCount)),
	}

	return result
}

func TestProxy(name string, proxy C.Proxy, downloadSize int, timeout time.Duration) (*Result, int64) {
	client := http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				host, port, err := net.SplitHostPort(addr)
				if err != nil {
					return nil, err
				}
				var u16Port uint16
				if port, err := strconv.ParseUint(port, 10, 16); err == nil {
					u16Port = uint16(port)
				}
				return proxy.DialContext(ctx, &C.Metadata{
					Host:    host,
					DstPort: u16Port,
				})
			},
		},
	}

	start := time.Now()
	resp, err := client.Get(fmt.Sprintf(*livenessObject, downloadSize))
	if err != nil {
		return &Result{name, -1, -1}, 0
	}
	defer func(Body io.ReadCloser) {
		err := Body.Close()
		if err != nil {

		}
	}(resp.Body)
	if resp.StatusCode-http.StatusOK > 100 {
		return &Result{name, -1, -1}, 0
	}
	ttfb := time.Since(start)

	written, _ := io.Copy(io.Discard, resp.Body)
	if written == 0 {
		return &Result{name, -1, -1}, 0
	}
	downloadTime := time.Since(start) - ttfb
	bandwidth := float64(written) / downloadTime.Seconds()

	return &Result{name, bandwidth, ttfb}, written
}

var (
	emojiRegex = regexp.MustCompile(`[\x{1F600}-\x{1F64F}\x{1F300}-\x{1F5FF}\x{1F680}-\x{1F6FF}\x{2600}-\x{26FF}\x{1F1E0}-\x{1F1FF}]`)
	spaceRegex = regexp.MustCompile(`\s{2,}`)
)

func formatName(name string) string {
	noEmoji := emojiRegex.ReplaceAllString(name, "")
	mergedSpaces := spaceRegex.ReplaceAllString(noEmoji, " ")
	return strings.TrimSpace(mergedSpaces)
}

func formatBandwidth(v float64) string {
	if v <= 0 {
		return "N/A"
	}
	if v < 1024 {
		return fmt.Sprintf("%.02fB/s", v)
	}
	v /= 1024
	if v < 1024 {
		return fmt.Sprintf("%.02fKB/s", v)
	}
	v /= 1024
	if v < 1024 {
		return fmt.Sprintf("%.02fMB/s", v)
	}
	v /= 1024
	if v < 1024 {
		return fmt.Sprintf("%.02fGB/s", v)
	}
	v /= 1024
	return fmt.Sprintf("%.02fTB/s", v)
}

func formatMilliseconds(v time.Duration) string {
	if v <= 0 {
		return "N/A"
	}
	return fmt.Sprintf("%.02fms", float64(v.Milliseconds()))
}

func writeNodeConfigurationToYAML(filePath string, results []Result, proxies map[string]CProxy) error {
	fp, err := os.Create(filePath)
	if err != nil {
		return err
	}
	defer func(fp *os.File) {
		err := fp.Close()
		if err != nil {

		}
	}(fp)

	var sortedProxies []any
	for _, result := range results {
		if v, ok := proxies[result.Name]; ok {
			sortedProxies = append(sortedProxies, v.SecretConfig)
		}
	}

	bytes, err := yaml.Marshal(sortedProxies)
	if err != nil {
		return err
	}

	_, err = fp.Write(bytes)
	return err
}

func writeToCSV(filePath string, results []Result) error {
	csvFile, err := os.Create(filePath)
	if err != nil {
		return err
	}
	defer func(csvFile *os.File) {
		err := csvFile.Close()
		if err != nil {

		}
	}(csvFile)

	// 写入 UTF-8 BOM 头
	_, err = csvFile.WriteString("\xEF\xBB\xBF")
	if err != nil {
		return err
	}

	csvWriter := csv.NewWriter(csvFile)
	err = csvWriter.Write([]string{"节点", "带宽 (MB/s)", "延迟 (ms)"})
	if err != nil {
		return err
	}
	for _, result := range results {
		line := []string{
			result.Name,
			fmt.Sprintf("%.2f", result.Bandwidth/1024/1024),
			strconv.FormatInt(result.TTFB.Milliseconds(), 10),
		}
		err = csvWriter.Write(line)
		if err != nil {
			return err
		}
	}
	csvWriter.Flush()
	return nil
}
