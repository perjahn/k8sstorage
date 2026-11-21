package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"html/template"
	"io"
	"log"
	"math/big"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/goccy/go-yaml"
)

const (
	requestTimeout = 30 * time.Second
	listPageLimit  = "500"
	maxConcurrent  = 8
	maxResponse    = 32 << 20
	noStorageClass = "(no storage class)"
)

type kubeConfig struct {
	Clusters       []namedCluster `yaml:"clusters"`
	Contexts       []namedContext `yaml:"contexts"`
	Users          []namedUser    `yaml:"users"`
	CurrentContext string         `yaml:"current-context"`
}

type namedCluster struct {
	Name    string      `yaml:"name"`
	Cluster clusterInfo `yaml:"cluster"`
}

type clusterInfo struct {
	Server                   string `yaml:"server"`
	CertificateAuthority     string `yaml:"certificate-authority"`
	CertificateAuthorityData string `yaml:"certificate-authority-data"`
	InsecureSkipTLSVerify    bool   `yaml:"insecure-skip-tls-verify"`
	TLSServerName            string `yaml:"tls-server-name"`
}

type namedContext struct {
	Name    string      `yaml:"name"`
	Context contextInfo `yaml:"context"`
}

type contextInfo struct {
	Cluster string `yaml:"cluster"`
	User    string `yaml:"user"`
}

type namedUser struct {
	Name string   `yaml:"name"`
	User userInfo `yaml:"user"`
}

type userInfo struct {
	Token                 string      `yaml:"token"`
	TokenFile             string      `yaml:"tokenFile"`
	Username              string      `yaml:"username"`
	Password              string      `yaml:"password"`
	ClientCertificate     string      `yaml:"client-certificate"`
	ClientCertificateData string      `yaml:"client-certificate-data"`
	ClientKey             string      `yaml:"client-key"`
	ClientKeyData         string      `yaml:"client-key-data"`
	Exec                  *execConfig `yaml:"exec"`
}

type execConfig struct {
	APIVersion string         `yaml:"apiVersion"`
	Command    string         `yaml:"command"`
	Args       []string       `yaml:"args"`
	Env        []execEnvEntry `yaml:"env"`
}

type execEnvEntry struct {
	Name  string `yaml:"name"`
	Value string `yaml:"value"`
}

type clusterTarget struct {
	name    string
	cluster clusterInfo
	user    userInfo
	baseDir string
	err     error
}

type clusterResult struct {
	name                  string
	storageByClass        map[string]*big.Rat
	pvcsByClass           map[string][]pvcDetail
	availableStorageClass map[string]struct{}
	defaultStorageClasses []string
	total                 *big.Rat
	err                   error
}

type clusterHeader struct {
	Name    string
	Error   string
	Tooltip string
}

type matrixRow struct {
	Class        string
	Tooltip      string
	Cells        []matrixCell
	Total        string
	TotalTooltip string
}

type matrixCell struct {
	Amount      string
	Unavailable bool
	Tooltip     string
}

type pageData struct {
	Clusters            []clusterHeader
	Rows                []matrixRow
	ClusterTotal        []string
	ClusterTotalTooltip []string
	GrandTotal          string
	GrandTotalTooltip   string
}

type pvcDetail struct {
	Namespace string
	Name      string
	Size      string
}

type pvcList struct {
	Metadata struct {
		Continue string `json:"continue"`
	} `json:"metadata"`
	Items []persistentVolumeClaim `json:"items"`
}

type storageClassList struct {
	Metadata struct {
		Continue string `json:"continue"`
	} `json:"metadata"`
	Items []storageClassItem `json:"items"`
}

type storageClassItem struct {
	Metadata struct {
		Name        string            `json:"name"`
		Annotations map[string]string `json:"annotations"`
	} `json:"metadata"`
}

type persistentVolumeClaim struct {
	Metadata pvcMetadata               `json:"metadata"`
	Spec     persistentVolumeClaimSpec `json:"spec"`
}

type pvcMetadata struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
}

type persistentVolumeClaimSpec struct {
	StorageClassName *string      `json:"storageClassName"`
	Resources        pvcResources `json:"resources"`
}

type pvcResources struct {
	Requests map[string]string `json:"requests"`
}

type execCredential struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Status     struct {
		Token                 string `json:"token"`
		ClientCertificateData string `json:"clientCertificateData"`
		ClientKeyData         string `json:"clientKeyData"`
	} `json:"status"`
}

type storageUnit struct {
	label string
	bytes *big.Int
}

var quantityPattern = regexp.MustCompile(`^([+-]?(?:[0-9]+(?:\.[0-9]*)?|\.[0-9]+))(?:[eE]([+-]?[0-9]+))?([a-zA-Z]*)$`)
var decimalQuantitySuffixes = map[string]int{
	"": 0, "n": -9, "u": -6, "m": -3, "k": 3, "K": 3,
	"M": 6, "G": 9, "T": 12, "P": 15, "E": 18,
}
var binaryQuantitySuffixes = map[string]uint{
	"Ki": 10, "Mi": 20, "Gi": 30, "Ti": 40, "Pi": 50, "Ei": 60,
}
var storageUnits = []storageUnit{
	{"Ei", new(big.Int).Lsh(big.NewInt(1), 60)},
	{"Pi", new(big.Int).Lsh(big.NewInt(1), 50)},
	{"Ti", new(big.Int).Lsh(big.NewInt(1), 40)},
	{"Gi", new(big.Int).Lsh(big.NewInt(1), 30)},
	{"Mi", new(big.Int).Lsh(big.NewInt(1), 20)},
	{"Ki", new(big.Int).Lsh(big.NewInt(1), 10)},
	{"B", big.NewInt(1)},
}

//go:embed index.html styles.css app.js
var webAssets embed.FS

func main() {
	home, err := os.UserHomeDir()
	if err != nil {
		log.Fatalf("resolve home directory: %v", err)
	}
	kubeconfig := flag.String("kubeconfig", envOrDefault("KUBECONFIG", filepath.Join(home, ".kube", "config")), "path to kubeconfig")
	address := flag.String("addr", envOrDefault("ADDR", "127.0.0.1:8080"), "HTTP listen address")
	excludePattern := flag.String("exclude-clusters", envOrDefault("EXCLUDE_CLUSTERS", ""), "regular expression matching cluster names to exclude")
	flag.Parse()

	var exclude *regexp.Regexp
	if *excludePattern != "" {
		exclude, err = regexp.Compile(*excludePattern)
		if err != nil {
			log.Fatalf("invalid -exclude-clusters regular expression: %v", err)
		}
	}
	targets, err := loadTargets(*kubeconfig, exclude)
	if err != nil {
		log.Fatal(err)
	}
	handler, err := newHandler(targets)
	if err != nil {
		log.Fatalf("prepare HTTP handler: %v", err)
	}

	server := &http.Server{
		Addr:              *address,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
	}
	log.Printf("Serving PVC storage matrix at http://%s/", *address)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}

func newHandler(targets []clusterTarget) (http.Handler, error) {
	pageTemplate, err := template.New("index.html").Funcs(template.FuncMap{
		"add": func(left, right int) int { return left + right },
		"len": func(values []clusterHeader) int { return len(values) },
	}).ParseFS(webAssets, "index.html")
	if err != nil {
		return nil, err
	}
	mux := http.NewServeMux()
	mux.Handle("GET /styles.css", http.FileServer(http.FS(webAssets)))
	mux.Handle("GET /app.js", http.FileServer(http.FS(webAssets)))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
		defer cancel()
		data := buildPageData(queryClusters(ctx, targets))
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		if err := pageTemplate.Execute(w, data); err != nil {
			log.Printf("render HTML: %v", err)
		}
	})
	return mux, nil
}

func envOrDefault(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

func loadTargets(path string, exclude *regexp.Regexp) ([]clusterTarget, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read kubeconfig %q: %w", path, err)
	}
	var config kubeConfig
	if err := yaml.Unmarshal(data, &config); err != nil {
		return nil, fmt.Errorf("parse kubeconfig %q: %w", path, err)
	}

	users := make(map[string]userInfo, len(config.Users))
	for _, entry := range config.Users {
		users[entry.Name] = entry.User
	}
	contextsByCluster := make(map[string][]namedContext)
	contextByName := make(map[string]namedContext, len(config.Contexts))
	for _, entry := range config.Contexts {
		contextsByCluster[entry.Context.Cluster] = append(contextsByCluster[entry.Context.Cluster], entry)
		contextByName[entry.Name] = entry
	}
	for cluster := range contextsByCluster {
		sort.Slice(contextsByCluster[cluster], func(i, j int) bool {
			return contextsByCluster[cluster][i].Name < contextsByCluster[cluster][j].Name
		})
	}

	clusterNames := make([]string, 0, len(config.Clusters))
	clusterByName := make(map[string]clusterInfo, len(config.Clusters))
	for _, entry := range config.Clusters {
		clusterByName[entry.Name] = entry.Cluster
		if exclude == nil || !exclude.MatchString(entry.Name) {
			clusterNames = append(clusterNames, entry.Name)
		}
	}
	sort.Strings(clusterNames)
	if len(clusterNames) == 0 {
		if len(config.Clusters) == 0 {
			return nil, errors.New("kubeconfig contains no clusters")
		}
		return nil, errors.New("all clusters were excluded by -exclude-clusters")
	}

	targets := make([]clusterTarget, 0, len(clusterNames))
	for _, name := range clusterNames {
		target := clusterTarget{name: name, cluster: clusterByName[name], baseDir: filepath.Dir(path)}
		parsed, err := url.Parse(target.cluster.Server)
		if err != nil || parsed.Host == "" || (parsed.Scheme != "https" && parsed.Scheme != "http") {
			return nil, fmt.Errorf("cluster %q has invalid API server URL %q", name, target.cluster.Server)
		}
		matchingContexts := contextsByCluster[name]
		if len(matchingContexts) == 0 {
			target.err = errors.New("no context references this cluster")
		} else {
			selectedContext := matchingContexts[0]
			if current, ok := contextByName[config.CurrentContext]; ok && current.Context.Cluster == name {
				selectedContext = current
			}
			target.user = users[selectedContext.Context.User]
		}
		targets = append(targets, target)
	}
	return targets, nil
}

func queryClusters(ctx context.Context, targets []clusterTarget) []clusterResult {
	results := make([]clusterResult, len(targets))
	semaphore := make(chan struct{}, maxConcurrent)
	var wait sync.WaitGroup
	for i, target := range targets {
		wait.Add(1)
		go func(i int, target clusterTarget) {
			defer wait.Done()
			select {
			case semaphore <- struct{}{}:
				defer func() { <-semaphore }()
			case <-ctx.Done():
				results[i] = clusterResult{name: target.name, err: ctx.Err()}
				return
			}
			results[i] = queryCluster(ctx, target)
		}(i, target)
	}
	wait.Wait()
	return results
}

func queryCluster(ctx context.Context, target clusterTarget) clusterResult {
	result := clusterResult{
		name:                  target.name,
		storageByClass:        make(map[string]*big.Rat),
		pvcsByClass:           make(map[string][]pvcDetail),
		availableStorageClass: make(map[string]struct{}),
		total:                 new(big.Rat),
	}
	if target.err != nil {
		result.err = target.err
		return result
	}
	client, headers, err := makeHTTPClient(ctx, target)
	if err != nil {
		result.err = fmt.Errorf("credentials: %w", err)
		return result
	}
	defer client.CloseIdleConnections()

	endpoint, err := url.Parse(strings.TrimRight(target.cluster.Server, "/") + "/api/v1/persistentvolumeclaims")
	if err != nil {
		result.err = fmt.Errorf("build PVC endpoint: %w", err)
		return result
	}
	continueToken := ""
	for {
		requestURL := *endpoint
		query := requestURL.Query()
		query.Set("limit", listPageLimit)
		if continueToken != "" {
			query.Set("continue", continueToken)
		}
		requestURL.RawQuery = query.Encode()

		var page pvcList
		if err := getJSON(ctx, client, headers, &requestURL, &page); err != nil {
			result.err = fmt.Errorf("list persistent volume claims: %w", err)
			return result
		}
		pageTotals, pageTotal, pagePVCs, err := aggregatePVCs(page.Items)
		if err != nil {
			result.err = fmt.Errorf("aggregate PVC page: %w", err)
			return result
		}
		for class, amount := range pageTotals {
			result.storageByClass[class] = sumQuantities(result.storageByClass[class], amount)
		}
		for class, pvcs := range pagePVCs {
			result.pvcsByClass[class] = append(result.pvcsByClass[class], pvcs...)
		}
		result.total = sumQuantities(result.total, pageTotal)
		continueToken = page.Metadata.Continue
		if continueToken == "" {
			break
		}
	}

	storageClassEndpoint, err := url.Parse(strings.TrimRight(target.cluster.Server, "/") + "/apis/storage.k8s.io/v1/storageclasses")
	if err != nil {
		result.err = fmt.Errorf("build storage class endpoint: %w", err)
		return result
	}
	continueToken = ""
	for {
		requestURL := *storageClassEndpoint
		query := requestURL.Query()
		query.Set("limit", listPageLimit)
		if continueToken != "" {
			query.Set("continue", continueToken)
		}
		requestURL.RawQuery = query.Encode()

		var page storageClassList
		if err := getJSON(ctx, client, headers, &requestURL, &page); err != nil {
			result.err = fmt.Errorf("list storage classes: %w", err)
			return result
		}
		for _, storageClass := range page.Items {
			result.availableStorageClass[storageClass.Metadata.Name] = struct{}{}
			if isDefaultStorageClass(storageClass.Metadata.Annotations) {
				result.defaultStorageClasses = append(result.defaultStorageClasses, storageClass.Metadata.Name)
			}
		}
		continueToken = page.Metadata.Continue
		if continueToken == "" {
			break
		}
	}
	sort.Strings(result.defaultStorageClasses)
	return result
}

func isDefaultStorageClass(annotations map[string]string) bool {
	if value, ok := annotations["storageclass.kubernetes.io/is-default-class"]; ok {
		return value == "true"
	}
	return annotations["storageclass.beta.kubernetes.io/is-default-class"] == "true"
}

func getJSON(ctx context.Context, client *http.Client, headers http.Header, requestURL *url.URL, result any) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, requestURL.String(), nil)
	if err != nil {
		return err
	}
	request.Header.Set("Accept", "application/json")
	for key, values := range headers {
		for _, value := range values {
			request.Header.Add(key, value)
		}
	}
	response, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("request %s: %w", requestURL.Redacted(), err)
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		message, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		closeErr := response.Body.Close()
		if closeErr != nil {
			return fmt.Errorf("API returned %s (%s); close response: %w", response.Status, strings.TrimSpace(string(message)), closeErr)
		}
		return fmt.Errorf("API returned %s: %s", response.Status, strings.TrimSpace(string(message)))
	}
	decodeErr := json.NewDecoder(io.LimitReader(response.Body, maxResponse)).Decode(result)
	closeErr := response.Body.Close()
	if decodeErr != nil {
		return fmt.Errorf("decode API response: %w", decodeErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close API response: %w", closeErr)
	}
	return nil
}

func makeHTTPClient(ctx context.Context, target clusterTarget) (*http.Client, http.Header, error) {
	tlsConfig := &tls.Config{
		MinVersion:         tls.VersionTLS12,
		InsecureSkipVerify: target.cluster.InsecureSkipTLSVerify,
		ServerName:         target.cluster.TLSServerName,
	}
	if target.cluster.CertificateAuthorityData != "" || target.cluster.CertificateAuthority != "" {
		caData, err := configBytes(target.cluster.CertificateAuthorityData, target.cluster.CertificateAuthority, target.baseDir)
		if err != nil {
			return nil, nil, fmt.Errorf("read certificate authority: %w", err)
		}
		roots := x509.NewCertPool()
		if !roots.AppendCertsFromPEM(caData) {
			return nil, nil, errors.New("certificate authority contains no valid certificates")
		}
		tlsConfig.RootCAs = roots
	}

	user := target.user
	headers := make(http.Header)
	var certificateData, keyData []byte
	if user.Exec != nil {
		credential, err := runExecCredential(ctx, user.Exec)
		if err != nil {
			return nil, nil, err
		}
		user.Token = credential.Status.Token
		if credential.Status.ClientCertificateData != "" || credential.Status.ClientKeyData != "" {
			certificateData = []byte(credential.Status.ClientCertificateData)
			keyData = []byte(credential.Status.ClientKeyData)
		}
	}
	if user.TokenFile != "" {
		token, err := os.ReadFile(resolvePath(target.baseDir, user.TokenFile))
		if err != nil {
			return nil, nil, fmt.Errorf("read token file: %w", err)
		}
		user.Token = strings.TrimSpace(string(token))
	}
	if user.Token != "" {
		headers.Set("Authorization", "Bearer "+user.Token)
	} else if user.Username != "" {
		headers.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(user.Username+":"+user.Password)))
	}

	if certificateData == nil {
		var err error
		certificateData, err = configBytes(user.ClientCertificateData, user.ClientCertificate, target.baseDir)
		if err != nil {
			return nil, nil, fmt.Errorf("read client certificate: %w", err)
		}
	}
	if keyData == nil {
		var err error
		keyData, err = configBytes(user.ClientKeyData, user.ClientKey, target.baseDir)
		if err != nil {
			return nil, nil, fmt.Errorf("read client key: %w", err)
		}
	}
	if len(certificateData) > 0 || len(keyData) > 0 {
		if len(certificateData) == 0 || len(keyData) == 0 {
			return nil, nil, errors.New("both client certificate and client key are required")
		}
		certificate, err := tls.X509KeyPair(certificateData, keyData)
		if err != nil {
			return nil, nil, fmt.Errorf("load client certificate: %w", err)
		}
		tlsConfig.Certificates = []tls.Certificate{certificate}
	}

	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = tlsConfig
	return &http.Client{Transport: transport, Timeout: requestTimeout}, headers, nil
}

func runExecCredential(ctx context.Context, config *execConfig) (execCredential, error) {
	var credential execCredential
	if config.Command == "" {
		return credential, errors.New("exec credential command is empty")
	}
	commandCtx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	cmd := exec.CommandContext(commandCtx, config.Command, config.Args...)
	cmd.Env = os.Environ()
	for _, entry := range config.Env {
		cmd.Env = append(cmd.Env, entry.Name+"="+entry.Value)
	}
	execInfo, _ := json.Marshal(map[string]any{
		"apiVersion": config.APIVersion,
		"kind":       "ExecCredential",
		"spec":       map[string]bool{"interactive": false},
	})
	cmd.Env = append(cmd.Env, "KUBERNETES_EXEC_INFO="+string(execInfo))
	output, err := cmd.Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return credential, fmt.Errorf("exec credential command failed: %s", strings.TrimSpace(string(exitErr.Stderr)))
		}
		return credential, fmt.Errorf("run exec credential command: %w", err)
	}
	if err := json.Unmarshal(output, &credential); err != nil {
		return credential, fmt.Errorf("parse exec credential output: %w", err)
	}
	if credential.APIVersion != config.APIVersion || credential.Kind != "ExecCredential" {
		return credential, fmt.Errorf("exec credential returned unexpected %s %s", credential.APIVersion, credential.Kind)
	}
	if credential.Status.Token == "" && (credential.Status.ClientCertificateData == "" || credential.Status.ClientKeyData == "") {
		return credential, errors.New("exec credential returned no token or client certificate")
	}
	return credential, nil
}

func configBytes(encoded, file, baseDir string) ([]byte, error) {
	if encoded != "" {
		data, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			return nil, fmt.Errorf("decode embedded data: %w", err)
		}
		return data, nil
	}
	if file == "" {
		return nil, nil
	}
	data, err := os.ReadFile(resolvePath(baseDir, file))
	if err != nil {
		return nil, err
	}
	return data, nil
}

func resolvePath(baseDir, path string) string {
	if filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(baseDir, path)
}

func aggregatePVCs(pvcs []persistentVolumeClaim) (map[string]*big.Rat, *big.Rat, map[string][]pvcDetail, error) {
	storageByClass := make(map[string]*big.Rat)
	pvcsByClass := make(map[string][]pvcDetail)
	total := new(big.Rat)
	for _, pvc := range pvcs {
		class := noStorageClass
		if pvc.Spec.StorageClassName != nil && *pvc.Spec.StorageClassName != "" {
			class = *pvc.Spec.StorageClassName
		}
		quantity, ok := pvc.Spec.Resources.Requests["storage"]
		if !ok {
			pvcsByClass[class] = append(pvcsByClass[class], pvcDetail{
				Namespace: pvc.Metadata.Namespace,
				Name:      pvc.Metadata.Name,
				Size:      "unspecified",
			})
			continue
		}
		amount, err := parseStorageQuantity(quantity)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("PVC %s/%s has invalid storage request %q: %w", pvc.Metadata.Namespace, pvc.Metadata.Name, quantity, err)
		}
		pvcsByClass[class] = append(pvcsByClass[class], pvcDetail{
			Namespace: pvc.Metadata.Namespace,
			Name:      pvc.Metadata.Name,
			Size:      formatQuantity(amount),
		})
		storageByClass[class] = sumQuantities(storageByClass[class], amount)
		total = sumQuantities(total, amount)
	}
	return storageByClass, total, pvcsByClass, nil
}

func parseStorageQuantity(value string) (*big.Rat, error) {
	parts := quantityPattern.FindStringSubmatch(value)
	if parts == nil {
		return nil, fmt.Errorf("unsupported Kubernetes quantity")
	}
	amount, ok := new(big.Rat).SetString(parts[1])
	if !ok {
		return nil, fmt.Errorf("invalid number")
	}
	if parts[2] != "" {
		exponent, err := strconv.Atoi(parts[2])
		if err != nil || exponent < -100 || exponent > 100 {
			return nil, fmt.Errorf("invalid exponent %q", parts[2])
		}
		power := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(abs(exponent))), nil)
		factor := new(big.Rat).SetInt(power)
		if exponent < 0 {
			amount.Quo(amount, factor)
		} else {
			amount.Mul(amount, factor)
		}
	}
	if amount.Sign() < 0 {
		return nil, errors.New("storage quantity cannot be negative")
	}

	if power, ok := decimalQuantitySuffixes[parts[3]]; ok {
		if power != 0 {
			factor := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(abs(power))), nil)
			scale := new(big.Rat).SetInt(factor)
			if power < 0 {
				amount.Quo(amount, scale)
			} else {
				amount.Mul(amount, scale)
			}
		}
		return amount, nil
	}
	if power, ok := binaryQuantitySuffixes[parts[3]]; ok {
		factor := new(big.Int).Lsh(big.NewInt(1), power)
		amount.Mul(amount, new(big.Rat).SetInt(factor))
		return amount, nil
	}
	return nil, fmt.Errorf("unsupported suffix %q", parts[3])
}

func abs(value int) int {
	if value < 0 {
		return -value
	}
	return value
}

func sumQuantities(left, right *big.Rat) *big.Rat {
	if left == nil {
		left = new(big.Rat)
	}
	if right == nil {
		right = new(big.Rat)
	}
	return new(big.Rat).Add(left, right)
}

func formatQuantity(quantity *big.Rat) string {
	if quantity == nil || quantity.Sign() == 0 {
		return "0.00 B"
	}
	absolute := new(big.Rat).Abs(quantity)
	for _, unit := range storageUnits {
		divisor := new(big.Rat).SetInt(unit.bytes)
		value := new(big.Rat).Quo(quantity, divisor)
		if absolute.Cmp(divisor) < 0 && unit.label != "B" {
			continue
		}
		return formatRat(value) + " " + unit.label
	}
	return formatRat(quantity) + " B"
}

func formatRat(value *big.Rat) string {
	return value.FloatString(2)
}

func buildPageData(results []clusterResult) pageData {
	classSet := make(map[string]struct{})
	for _, result := range results {
		if result.err != nil {
			continue
		}
		for class := range result.storageByClass {
			classSet[class] = struct{}{}
		}
		for class := range result.pvcsByClass {
			classSet[class] = struct{}{}
		}
		for class := range result.availableStorageClass {
			classSet[class] = struct{}{}
		}
	}
	classes := make([]string, 0, len(classSet))
	for class := range classSet {
		classes = append(classes, class)
	}
	sort.Strings(classes)

	defaultsByCluster := make([]string, len(results))
	for i, result := range results {
		defaultsByCluster[i] = result.name + ": " + defaultStorageClassLabel(result)
	}
	defaultsTooltip := strings.Join(defaultsByCluster, "\n")

	data := pageData{
		Clusters:            make([]clusterHeader, len(results)),
		Rows:                make([]matrixRow, 0, len(classes)),
		ClusterTotal:        make([]string, len(results)),
		ClusterTotalTooltip: make([]string, len(results)),
		GrandTotal:          formatQuantity(new(big.Rat)),
		GrandTotalTooltip:   allPVCsTooltip(results),
	}
	grandTotal := new(big.Rat)
	for i, result := range results {
		data.Clusters[i] = clusterHeader{
			Name:    result.name,
			Tooltip: clusterHeaderTooltip(result),
		}
		if result.err != nil {
			data.Clusters[i].Error = result.err.Error()
			data.ClusterTotal[i] = "—"
			data.ClusterTotalTooltip[i] = "Cluster unavailable: " + result.err.Error()
		} else {
			grandTotal = sumQuantities(grandTotal, result.total)
			data.ClusterTotal[i] = formatQuantity(result.total)
			data.ClusterTotalTooltip[i] = clusterPVCsTooltip(result)
		}
	}

	for _, class := range classes {
		row := matrixRow{Class: class, Cells: make([]matrixCell, len(results))}
		if class == noStorageClass {
			row.Tooltip = defaultsTooltip
		}
		classTotal := new(big.Rat)
		for i, result := range results {
			if result.err != nil {
				row.Cells[i].Amount = "—"
				row.Cells[i].Tooltip = "Cluster unavailable: " + result.err.Error()
				continue
			}
			amount, exists := result.storageByClass[class]
			if exists {
				row.Cells[i].Amount = formatQuantity(amount)
				classTotal = sumQuantities(classTotal, amount)
			} else {
				row.Cells[i].Amount = "—"
			}
			_, available := result.availableStorageClass[class]
			row.Cells[i].Unavailable = class != noStorageClass &&
				result.availableStorageClass != nil && !available
			row.Cells[i].Tooltip = pvcDetailsTooltip(result.pvcsByClass[class], "")
			if row.Cells[i].Tooltip == "" {
				row.Cells[i].Tooltip = "No PVCs"
			}
			if row.Cells[i].Unavailable {
				row.Cells[i].Tooltip = "Storage class unavailable\n" + row.Cells[i].Tooltip
			}
		}
		row.Total = formatQuantity(classTotal)
		row.TotalTooltip = classPVCsTooltip(results, class)
		data.Rows = append(data.Rows, row)
	}
	data.GrandTotal = formatQuantity(grandTotal)
	return data
}

func pvcDetailsTooltip(details []pvcDetail, prefix string) string {
	if len(details) == 0 {
		return ""
	}
	sorted := append([]pvcDetail(nil), details...)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].Namespace == sorted[j].Namespace {
			return sorted[i].Name < sorted[j].Name
		}
		return sorted[i].Namespace < sorted[j].Namespace
	})
	lines := make([]string, len(sorted))
	for i, detail := range sorted {
		lines[i] = fmt.Sprintf("%s%s/%s: %s", prefix, detail.Namespace, detail.Name, detail.Size)
	}
	return strings.Join(lines, "\n")
}

func clusterPVCsTooltip(result clusterResult) string {
	if result.err != nil {
		return "Cluster unavailable: " + result.err.Error()
	}
	classes := make([]string, 0, len(result.pvcsByClass))
	for class := range result.pvcsByClass {
		classes = append(classes, class)
	}
	sort.Strings(classes)
	var lines []string
	for _, class := range classes {
		lines = appendTooltipLines(lines, pvcDetailsTooltip(result.pvcsByClass[class], class+" — "))
	}
	if len(lines) == 0 {
		return "No PVCs"
	}
	return strings.Join(lines, "\n")
}

func classPVCsTooltip(results []clusterResult, class string) string {
	var lines []string
	for _, result := range results {
		if result.err != nil {
			lines = append(lines, result.name+": unavailable")
			continue
		}
		details := pvcDetailsTooltip(result.pvcsByClass[class], result.name+" — ")
		if details == "" {
			lines = append(lines, result.name+": No PVCs")
			continue
		}
		lines = append(lines, details)
	}
	if len(lines) == 0 {
		return "No PVCs"
	}
	return strings.Join(lines, "\n")
}

func allPVCsTooltip(results []clusterResult) string {
	var lines []string
	for _, result := range results {
		if result.err != nil {
			lines = append(lines, result.name+": unavailable")
			continue
		}
		classes := make([]string, 0, len(result.pvcsByClass))
		for class := range result.pvcsByClass {
			classes = append(classes, class)
		}
		sort.Strings(classes)
		clusterLines := make([]string, 0)
		for _, class := range classes {
			clusterLines = appendTooltipLines(clusterLines,
				pvcDetailsTooltip(result.pvcsByClass[class], result.name+" — "+class+" — "))
		}
		if len(clusterLines) == 0 {
			lines = append(lines, result.name+": No PVCs")
			continue
		}
		lines = append(lines, clusterLines...)
	}
	if len(lines) == 0 {
		return "No PVCs"
	}
	return strings.Join(lines, "\n")
}

func appendTooltipLines(lines []string, tooltip string) []string {
	if tooltip == "" {
		return lines
	}
	return append(lines, strings.Split(tooltip, "\n")...)
}

func clusterHeaderTooltip(result clusterResult) string {
	defaultClass := defaultStorageClassLabel(result)
	tooltip := "Default storage class: " + defaultClass
	if result.err != nil {
		tooltip += "\n" + result.err.Error()
	}
	return tooltip
}

func defaultStorageClassLabel(result clusterResult) string {
	if result.err != nil {
		return "unknown"
	}
	if len(result.defaultStorageClasses) == 0 {
		return "none"
	}
	return strings.Join(result.defaultStorageClasses, ", ")
}
