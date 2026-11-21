package main

import (
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestBuildPageDataIncludesClassAndGrandTotals(t *testing.T) {
	results := []clusterResult{
		{
			name: "alpha",
			storageByClass: map[string]*big.Rat{
				"fast": mustParseQuantity(t, "1Gi"),
				"slow": mustParseQuantity(t, "512Mi"),
			},
			total: mustParseQuantity(t, "1536Mi"),
		},
		{
			name: "beta",
			storageByClass: map[string]*big.Rat{
				"fast": mustParseQuantity(t, "2Gi"),
			},
			total: mustParseQuantity(t, "2Gi"),
		},
	}

	data := buildPageData(results)
	if got, want := len(data.Rows), 2; got != want {
		t.Fatalf("got %d rows, want %d", got, want)
	}
	if data.Rows[0].Class != "fast" || data.Rows[0].Cells[0].Amount != "1.00 Gi" || data.Rows[0].Cells[1].Amount != "2.00 Gi" || data.Rows[0].Total != "3.00 Gi" {
		t.Errorf("fast row = %#v, want amounts 1.00 Gi, 2.00 Gi and total 3.00 Gi", data.Rows[0])
	}
	if data.Rows[1].Class != "slow" || data.Rows[1].Cells[1].Amount != "—" || data.Rows[1].Total != "512.00 Mi" {
		t.Errorf("slow row = %#v, want beta dash and total 512.00 Mi", data.Rows[1])
	}
	if strings.Join(data.ClusterTotal, ",") != "1.50 Gi,2.00 Gi" {
		t.Errorf("cluster totals = %v, want 1.50 Gi,2.00 Gi", data.ClusterTotal)
	}
	if data.GrandTotal != "3.50 Gi" {
		t.Errorf("grand total = %q, want 3.50 Gi", data.GrandTotal)
	}
}

func TestBuildPageDataMarksFailedClusterUnavailableAndExcludesItFromTotals(t *testing.T) {
	data := buildPageData([]clusterResult{
		{
			name:           "available",
			storageByClass: map[string]*big.Rat{"gold": mustParseQuantity(t, "4Gi")},
			total:          mustParseQuantity(t, "4Gi"),
		},
		{name: "offline", err: errors.New("unreachable")},
	})

	if data.Clusters[1].Error == "" || data.ClusterTotal[1] != "—" {
		t.Fatalf("failed cluster should be marked unavailable: %#v", data.Clusters[1])
	}
	if data.Rows[0].Cells[1].Amount != "—" || data.Rows[0].Total != "4.00 Gi" || data.GrandTotal != "4.00 Gi" {
		t.Errorf("failed cluster affected totals: row=%#v grand total=%q", data.Rows[0], data.GrandTotal)
	}
}

func TestBuildPageDataIncludesUnusedStorageClassesAndMarksUnavailableCells(t *testing.T) {
	data := buildPageData([]clusterResult{
		{
			name: "alpha",
			storageByClass: map[string]*big.Rat{
				"fast": mustParseQuantity(t, "1Gi"),
				"old":  mustParseQuantity(t, "256Mi"),
			},
			availableStorageClass: map[string]struct{}{
				"fast":   {},
				"unused": {},
			},
			defaultStorageClasses: []string{"fast"},
		},
		{
			name: "beta",
			availableStorageClass: map[string]struct{}{
				"fast": {},
			},
		},
	})

	rows := make(map[string]matrixRow, len(data.Rows))
	for _, row := range data.Rows {
		rows[row.Class] = row
	}
	if got := rows["unused"].Cells[0]; got.Amount != "—" || got.Unavailable {
		t.Errorf("available but unused class cell = %#v, want unhighlighted dash", got)
	}
	if got := rows["unused"].Cells[1]; got.Amount != "—" || !got.Unavailable {
		t.Errorf("missing class cell = %#v, want highlighted dash", got)
	}
	if got := rows["old"].Cells[0]; got.Amount != "256.00 Mi" || !got.Unavailable {
		t.Errorf("used but unavailable class cell = %#v, want highlighted amount", got)
	}
	if !strings.Contains(data.Clusters[0].Tooltip, "Default storage class: fast") {
		t.Errorf("cluster tooltip = %q, want default class fast", data.Clusters[0].Tooltip)
	}
}

func TestNoStorageClassRowHeaderShowsAllClusterDefaultsInTooltip(t *testing.T) {
	data := buildPageData([]clusterResult{
		{
			name:                  "alpha",
			storageByClass:        map[string]*big.Rat{noStorageClass: mustParseQuantity(t, "2Gi")},
			defaultStorageClasses: []string{"fast"},
		},
		{
			name:                  "beta",
			storageByClass:        map[string]*big.Rat{noStorageClass: mustParseQuantity(t, "1Gi")},
			defaultStorageClasses: []string{"standard"},
		},
	})

	row := data.Rows[0]
	if row.Class != noStorageClass {
		t.Fatalf("first row = %q, want %q", row.Class, noStorageClass)
	}
	want := "alpha: fast\nbeta: standard"
	if row.Tooltip != want {
		t.Errorf("row tooltip = %q, want %q", row.Tooltip, want)
	}
}

func TestIsDefaultStorageClassSupportsStableAndLegacyAnnotations(t *testing.T) {
	tests := []struct {
		name        string
		annotations map[string]string
		want        bool
	}{
		{name: "stable annotation", annotations: map[string]string{"storageclass.kubernetes.io/is-default-class": "true"}, want: true},
		{name: "legacy annotation", annotations: map[string]string{"storageclass.beta.kubernetes.io/is-default-class": "true"}, want: true},
		{name: "stable false overrides legacy true", annotations: map[string]string{
			"storageclass.kubernetes.io/is-default-class":      "false",
			"storageclass.beta.kubernetes.io/is-default-class": "true",
		}},
		{name: "not default", annotations: map[string]string{}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := isDefaultStorageClass(test.annotations); got != test.want {
				t.Errorf("isDefaultStorageClass() = %t, want %t", got, test.want)
			}
		})
	}
}

func TestAggregatePVCsGroupsRequestedStorageByClass(t *testing.T) {
	fast := "fast"
	pvcs := []persistentVolumeClaim{
		{Metadata: pvcMetadata{Name: "fast-1", Namespace: "ns"}, Spec: persistentVolumeClaimSpec{
			StorageClassName: &fast,
			Resources:        pvcResources{Requests: map[string]string{"storage": "1Gi"}},
		}},
		{Metadata: pvcMetadata{Name: "fast-2", Namespace: "ns"}, Spec: persistentVolumeClaimSpec{
			StorageClassName: &fast,
			Resources:        pvcResources{Requests: map[string]string{"storage": "512Mi"}},
		}},
		{Metadata: pvcMetadata{Name: "unclassified", Namespace: "ns"}, Spec: persistentVolumeClaimSpec{
			Resources: pvcResources{Requests: map[string]string{"storage": "2Gi"}},
		}},
	}
	byClass, total, pvcsByClass, err := aggregatePVCs(pvcs)
	if err != nil {
		t.Fatal(err)
	}
	if got := formatQuantity(byClass["fast"]); got != "1.50 Gi" {
		t.Errorf("fast storage = %q, want 1.50 Gi", got)
	}
	if got := formatQuantity(byClass[noStorageClass]); got != "2.00 Gi" {
		t.Errorf("no-class storage = %q, want 2.00 Gi", got)
	}
	if got := formatQuantity(total); got != "3.50 Gi" {
		t.Errorf("total storage = %q, want 3.50 Gi", got)
	}
	if got := pvcDetailsTooltip(pvcsByClass["fast"], ""); got != "ns/fast-1: 1.00 Gi\nns/fast-2: 512.00 Mi" {
		t.Errorf("PVC tooltip = %q, want both PVC sizes on separate lines", got)
	}
}

func TestParseStorageQuantitySupportsDecimalAndBinaryUnits(t *testing.T) {
	for input, want := range map[string]string{
		"1.5Gi": "1610612736",
		"2G":    "2000000000",
		"500m":  "1/2",
		"0.5n":  "1/2000000000",
	} {
		quantity, err := parseStorageQuantity(input)
		if err != nil {
			t.Errorf("parseStorageQuantity(%q): %v", input, err)
			continue
		}
		if got := quantity.RatString(); got != want {
			t.Errorf("parseStorageQuantity(%q) = %s, want %s", input, got, want)
		}
	}
	if got := formatQuantity(mustParseQuantity(t, "10")); got != "10.00 B" {
		t.Errorf("formatQuantity(10) = %q, want 10.00 B", got)
	}
	if got := formatQuantity(mustParseQuantity(t, "1.234Gi")); got != "1.23 Gi" {
		t.Errorf("formatQuantity(1.234Gi) = %q, want 1.23 Gi", got)
	}
	if got := formatQuantity(mustParseQuantity(t, "1.235Gi")); got != "1.24 Gi" {
		t.Errorf("formatQuantity(1.235Gi) = %q, want 1.24 Gi", got)
	}
	if _, err := parseStorageQuantity("-1Gi"); err == nil {
		t.Error("parseStorageQuantity accepted a negative storage request")
	}
}

func TestLoadTargetsSelectsCurrentContextAndSortsFallbackContexts(t *testing.T) {
	config := `apiVersion: v1
current-context: chosen
clusters:
- name: first
  cluster:
    server: https://first.example
- name: second
  cluster:
    server: https://second.example
contexts:
- name: chosen
  context:
    cluster: first
    user: preferred
- name: alternate
  context:
    cluster: first
    user: alternate
- name: z-context
  context:
    cluster: second
    user: second-z
- name: a-context
  context:
    cluster: second
    user: second-a
users:
- name: preferred
  user:
    token: preferred-token
- name: alternate
  user:
    token: alternate-token
- name: second-a
  user:
    token: second-token
`
	path := filepath.Join(t.TempDir(), "config")
	if err := os.WriteFile(path, []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	targets, err := loadTargets(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(targets) != 2 {
		t.Fatalf("got %d targets, want 2", len(targets))
	}
	if targets[0].user.Token != "preferred-token" {
		t.Errorf("first cluster token = %q, want current-context token", targets[0].user.Token)
	}
	if targets[1].user.Token != "second-token" {
		t.Errorf("second cluster token = %q, want token from lexically first context", targets[1].user.Token)
	}
}

func TestHandlerRendersPVCMatrixUsingPlainHTTPAndPagination(t *testing.T) {
	pvcRequests := 0
	storageClassRequests := 0
	apiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("limit") != listPageLimit {
			t.Errorf("list limit = %q, want %s", r.URL.Query().Get("limit"), listPageLimit)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
			t.Errorf("authorization = %q, want bearer token", got)
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/persistentvolumeclaims":
			pvcRequests++
			if r.URL.Query().Get("continue") == "" {
				fmt.Fprint(w, `{"metadata":{"continue":"next page"},"items":[{"metadata":{"name":"first","namespace":"default"},"spec":{"storageClassName":"fast","resources":{"requests":{"storage":"1Gi"}}}}]}`)
				return
			}
			if r.URL.Query().Get("continue") != "next page" {
				t.Errorf("PVC continue token = %q, want next page", r.URL.Query().Get("continue"))
			}
			fmt.Fprint(w, `{"metadata":{},"items":[{"metadata":{"name":"second-fast","namespace":"default"},"spec":{"storageClassName":"fast","resources":{"requests":{"storage":"512Mi"}}}},{"metadata":{"name":"second","namespace":"default"},"spec":{"storageClassName":"old","resources":{"requests":{"storage":"256Mi"}}}},{"metadata":{"name":"third","namespace":"default"},"spec":{"resources":{"requests":{"storage":"512Mi"}}}}]}`)
		case "/apis/storage.k8s.io/v1/storageclasses":
			storageClassRequests++
			if r.URL.Query().Get("continue") == "" {
				fmt.Fprint(w, `{"metadata":{"continue":"next class page"},"items":[{"metadata":{"name":"fast","annotations":{"storageclass.kubernetes.io/is-default-class":"true"}}},{"metadata":{"name":"unused"}}]}`)
				return
			}
			if r.URL.Query().Get("continue") != "next class page" {
				t.Errorf("storage class continue token = %q, want next class page", r.URL.Query().Get("continue"))
			}
			fmt.Fprint(w, `{"metadata":{},"items":[{"metadata":{"name":"standard","annotations":{"storageclass.beta.kubernetes.io/is-default-class":"true"}}}]}`)
		default:
			t.Errorf("unexpected request path %q", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer apiServer.Close()

	handler, err := newHandler([]clusterTarget{{
		name:    "test",
		cluster: clusterInfo{Server: apiServer.URL},
		user:    userInfo{Token: "test-token"},
	}})
	if err != nil {
		t.Fatal(err)
	}

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", response.Code, response.Body.String())
	}
	if pvcRequests != 2 || storageClassRequests != 2 {
		t.Fatalf("PVC requests = %d, storage class requests = %d; want 2 pages each", pvcRequests, storageClassRequests)
	}
	body := response.Body.String()
	for _, expected := range []string{
		"/app.js",
		"All clusters",
		"All storage classes",
		"title=\"Default storage class: fast, standard\"",
		`<th scope="row" title="test: fast, standard">(no storage class)</th>`,
		`title="default/first: 1.00 Gi
default/second-fast: 512.00 Mi"`,
		`title="Storage class unavailable
default/second: 256.00 Mi"`,
		`title="test — default/first: 1.00 Gi`,
		`title="test — (no storage class) — default/third: 512.00 Mi`,
		"fast",
		"second-fast",
		"old",
		"standard",
		"unused",
		noStorageClass,
		"1.00 Gi",
		"256.00 Mi",
		"512.00 Mi",
		"1.50 Gi",
		"2.25 Gi",
		`class="class-unavailable"`,
	} {
		if !strings.Contains(body, expected) {
			t.Errorf("rendered matrix is missing %q: %s", expected, body)
		}
	}

	cssResponse := httptest.NewRecorder()
	handler.ServeHTTP(cssResponse, httptest.NewRequest(http.MethodGet, "/styles.css", nil))
	if cssResponse.Code != http.StatusOK ||
		!strings.Contains(cssResponse.Body.String(), "table {") ||
		!strings.Contains(cssResponse.Body.String(), ".class-unavailable") {
		t.Errorf("stylesheet response = status %d, body %q", cssResponse.Code, cssResponse.Body.String())
	}

	jsResponse := httptest.NewRecorder()
	handler.ServeHTTP(jsResponse, httptest.NewRequest(http.MethodGet, "/app.js", nil))
	if jsResponse.Code != http.StatusOK || !strings.Contains(jsResponse.Body.String(), "storageValue") {
		t.Errorf("JavaScript response = status %d, body %q", jsResponse.Code, jsResponse.Body.String())
	}
}

func mustParseQuantity(t *testing.T, value string) *big.Rat {
	t.Helper()
	quantity, err := parseStorageQuantity(value)
	if err != nil {
		t.Fatalf("parse quantity %q: %v", value, err)
	}
	return quantity
}

func TestLoadTargetsSupportsClusterExclusion(t *testing.T) {
	config := `clusters:
- name: prod
  cluster:
    server: https://prod.example
- name: dev
  cluster:
    server: https://dev.example
contexts:
- name: dev
  context:
    cluster: dev
`
	path := filepath.Join(t.TempDir(), "config")
	if err := os.WriteFile(path, []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	targets, err := loadTargets(path, regexp.MustCompile("prod"))
	if err != nil {
		t.Fatal(err)
	}
	if len(targets) != 1 || targets[0].name != "dev" {
		t.Errorf("targets = %#v, want only dev", targets)
	}
}
