package apitest

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/rest"

	sandboxv1alpha1 "github.com/psenna/ai-sandbox/operator/api/v1alpha1"
)

// getTable issues a raw GET against path with the Table content negotiation
// header and unmarshals the response as a metav1.Table.
func getTable(t *testing.T, path string) *metav1.Table {
	t.Helper()

	httpClient, err := rest.HTTPClientFor(k8sCfg)
	if err != nil {
		t.Fatalf("rest.HTTPClientFor: %v", err)
	}

	req, err := http.NewRequest(http.MethodGet, strings.TrimRight(k8sCfg.Host, "/")+path, nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Accept", "application/json;as=Table;v=v1;g=meta.k8s.io")

	resp, err := httpClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading response body: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: status %d, body: %s", path, resp.StatusCode, body)
	}

	table := &metav1.Table{}
	if err := json.Unmarshal(body, table); err != nil {
		t.Fatalf("unmarshaling Table: %v (body: %s)", err, body)
	}
	return table
}

func TestPrinterColumns_SandboxEnvironment(t *testing.T) {
	env := validEnv("printercol-env")
	env.Spec.ClassRef.Name = "printercol-class"
	env.Spec.Repo = "psenna/printercol-repo"
	if err := k8s.Create(ctx, env); err != nil {
		t.Fatalf("Create: %v", err)
	}

	current := &sandboxv1alpha1.SandboxEnvironment{}
	if err := k8s.Get(ctx, types.NamespacedName{Name: env.Name, Namespace: env.Namespace}, current); err != nil {
		t.Fatalf("Get: %v", err)
	}
	current.Status.Phase = sandboxv1alpha1.PhaseRunning
	current.Status.Slot.Granted = true
	current.Status.FreezeCount = 3
	if err := k8s.Status().Update(ctx, current); err != nil {
		t.Fatalf("Status().Update: %v", err)
	}

	table := getTable(t, fmt.Sprintf("/apis/%s/%s/namespaces/%s/sandboxenvironments", sandboxv1alpha1.GroupVersion.Group, sandboxv1alpha1.GroupVersion.Version, testNamespace))

	wantCols := []string{"Name", "Phase", "Slot", "Freezes", "Age", "Class", "Repo"}
	if len(table.ColumnDefinitions) != len(wantCols) {
		t.Fatalf("ColumnDefinitions = %v, want %v", colNames(table), wantCols)
	}
	for i, name := range wantCols {
		if table.ColumnDefinitions[i].Name != name {
			t.Errorf("ColumnDefinitions[%d].Name = %q, want %q", i, table.ColumnDefinitions[i].Name, name)
		}
	}

	var row *metav1.TableRow
	for i := range table.Rows {
		if len(table.Rows[i].Cells) > 0 && fmt.Sprint(table.Rows[i].Cells[0]) == env.Name {
			row = &table.Rows[i]
			break
		}
	}
	if row == nil {
		t.Fatalf("no row found for %q in table with %d rows", env.Name, len(table.Rows))
	}

	cells := row.Cells
	if got := fmt.Sprint(cells[1]); got != string(sandboxv1alpha1.PhaseRunning) {
		t.Errorf("Phase cell = %q, want %q", got, sandboxv1alpha1.PhaseRunning)
	}
	if got := fmt.Sprint(cells[2]); got != "true" {
		t.Errorf("Slot cell = %q, want %q", got, "true")
	}
	if got := fmt.Sprint(cells[3]); got != "3" {
		t.Errorf("Freezes cell = %q, want %q", got, "3")
	}
	if got := fmt.Sprint(cells[5]); got != "printercol-class" {
		t.Errorf("Class cell = %q, want %q", got, "printercol-class")
	}
	if got := fmt.Sprint(cells[6]); got != "psenna/printercol-repo" {
		t.Errorf("Repo cell = %q, want %q", got, "psenna/printercol-repo")
	}
}

func colNames(table *metav1.Table) []string {
	names := make([]string, len(table.ColumnDefinitions))
	for i, c := range table.ColumnDefinitions {
		names[i] = c.Name
	}
	return names
}

func TestDiscovery_ResourceMetadata(t *testing.T) {
	dc, err := discovery.NewDiscoveryClientForConfig(k8sCfg)
	if err != nil {
		t.Fatalf("NewDiscoveryClientForConfig: %v", err)
	}

	list, err := dc.ServerResourcesForGroupVersion(sandboxv1alpha1.GroupVersion.String())
	if err != nil {
		t.Fatalf("ServerResourcesForGroupVersion: %v", err)
	}

	byName := map[string]metav1.APIResource{}
	for _, r := range list.APIResources {
		byName[r.Name] = r
	}

	env, ok := byName["sandboxenvironments"]
	if !ok {
		t.Fatalf("sandboxenvironments not found in %v", byName)
	}
	if !containsStr(env.ShortNames, "sbenv") {
		t.Errorf("sandboxenvironments ShortNames = %v, want to contain %q", env.ShortNames, "sbenv")
	}
	if !containsStr(env.Categories, "sandbox") {
		t.Errorf("sandboxenvironments Categories = %v, want to contain %q", env.Categories, "sandbox")
	}
	if !env.Namespaced {
		t.Errorf("sandboxenvironments Namespaced = false, want true")
	}

	class, ok := byName["sandboxclasses"]
	if !ok {
		t.Fatalf("sandboxclasses not found in %v", byName)
	}
	if !containsStr(class.ShortNames, "sbclass") {
		t.Errorf("sandboxclasses ShortNames = %v, want to contain %q", class.ShortNames, "sbclass")
	}
	if !containsStr(class.Categories, "sandbox") {
		t.Errorf("sandboxclasses Categories = %v, want to contain %q", class.Categories, "sandbox")
	}
	if class.Namespaced {
		t.Errorf("sandboxclasses Namespaced = true, want false")
	}
}

func containsStr(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
