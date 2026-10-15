package app

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteCatalogSortsByLowerCaseNameThenID(t *testing.T) {
	rows := map[string]catRow{
		"3": {id: "3", name: "zelda.gb", size: 1},
		"1": {id: "1", name: "Zelda.gb", size: 2}, // same name ignoring case: the id decides
		"2": {id: "2", name: "alpha.gb", size: 3},
		"4": {id: "4", name: "Beta.gb", size: 4},
		"5": {id: "5", name: "Éclair.gb", size: 5},
	}
	p := filepath.Join(t.TempDir(), "c.tsv")
	if err := writeCatalog(p, rows); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p)
	var order []string
	for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		order = append(order, strings.SplitN(l, "\x1f", 3)[1])
	}
	want := "alpha.gb,Beta.gb,Zelda.gb,zelda.gb,Éclair.gb"
	if got := strings.Join(order, ","); got != want {
		t.Errorf("order = %s, want %s", got, want)
	}
	if back := readCatalog(p); len(back) != 5 || back["1"].size != 2 {
		t.Errorf("round trip = %v", back)
	}
}

func testModel(t *testing.T) *model {
	t.Helper()
	d := &Data{Config: map[string]string{"server_url": "http://x"}, Root: t.TempDir()}
	m := newModel(d, newTheme(true, "auto"))
	m.pop = nil
	return m
}

func TestHTTPTransportsAndCertificatesAreSharedAcrossRequests(t *testing.T) {
	m := testModel(t)
	a, b := m.env(), m.env()
	if a.client.Transport != b.client.Transport || a.client.Transport != a.longClient.Transport || a.client.Transport != m.client().Transport {
		t.Fatal("every request of a session must reuse the same transport (and so its idle connections)")
	}
	p1, _ := m.caPool()
	p2, _ := m.caPool()
	if p1 != p2 {
		t.Error("the certificate pool must be parsed once")
	}

	// the transport that skips certificate checks is another one, shared in the same way
	m.data.Config["verify_tls"] = "0"
	c := m.env()
	if c.client.Transport == a.client.Transport || c.client.Transport != m.env().client.Transport {
		t.Error("insecure mode needs its own shared transport")
	}
	if !c.client.Transport.(*http.Transport).TLSClientConfig.InsecureSkipVerify {
		t.Error("the insecure transport must skip verification")
	}
	m.data.Config["verify_tls"] = "1"
	if m.env().client.Transport != a.client.Transport {
		t.Error("going back to verified mode must reuse the verified transport")
	}

	// changing the certificate file setting builds a new pool and new transports
	m.data.Config["ca_file"] = filepath.Join(t.TempDir(), "missing.pem")
	if n := m.env(); n.client.Transport == a.client.Transport {
		t.Error("a new ca_file must not keep the old transport")
	}
}

func BenchmarkEnv(b *testing.B) {
	m := bigModel(100)
	for i := 0; i < b.N; i++ {
		_ = m.env()
	}
}

func BenchmarkWriteCatalog40k(b *testing.B) {
	rows := map[string]catRow{}
	for i := 0; i < 40000; i++ {
		rows[fmt.Sprint(i)] = catRow{id: fmt.Sprint(i), name: fmt.Sprintf("Some Game Number %d (USA).sfc", (i*7919)%40000), size: int64(i), sha1: "0123456789abcdef0123456789abcdef01234567"}
	}
	p := filepath.Join(b.TempDir(), "c.tsv")
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = writeCatalog(p, rows)
	}
}
