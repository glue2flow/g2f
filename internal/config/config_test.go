package config

import "testing"

func TestParseProxies(t *testing.T) {
	got, err := parseProxies("10.0.0.0/8, 127.0.0.1 ,::1,10.1.2.3/8")
	if err != nil || len(got) != 4 {
		t.Fatalf("%v %v", got, err)
	}
	want := []string{"10.0.0.0/8", "127.0.0.1/32", "::1/128", "10.0.0.0/8"}
	for i, p := range got {
		if p.String() != want[i] {
			t.Errorf("%d: %s, want %s", i, p, want[i])
		}
	}
	if got, err := parseProxies(""); err != nil || got != nil {
		t.Fatalf("empty: %v %v", got, err)
	}
	if _, err := parseProxies("10.0.0.0/8, nope"); err == nil {
		t.Fatal("expected error")
	}
}
