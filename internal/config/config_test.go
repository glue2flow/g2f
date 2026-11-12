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

func TestCheckDemoKeys(t *testing.T) {
	demo := demoSecretKeys[0]
	makefileKey := demoSecretKeys[1]
	const own = "b3duLWtleS1mb3ItZzJmLXRlc3RzLW9ubHkhIQ=="
	tests := []struct {
		name    string
		key     string
		old     []string
		url     string
		wantErr bool
	}{
		{"demo key localhost", demo, nil, "http://localhost:8080", false},
		{"demo key 127.0.0.1", demo, nil, "http://127.0.0.1:8080", false},
		{"demo key ::1", demo, nil, "http://[::1]:8080", false},
		{"demo key public host", demo, nil, "https://g2f.example.com", true},
		{"makefile key public host", makefileKey, nil, "https://g2f.example.com", true},
		{"own key public host", own, nil, "https://g2f.example.com", false},
		{"demo key in old keys public host", own, []string{demo}, "https://g2f.example.com", true},
		{"demo key localhost suffix lookalike", demo, nil, "http://localhost.evil.com", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := &Config{SecretKey: tt.key, SecretKeysOld: tt.old, PublicURL: tt.url}
			if err := checkDemoKeys(c); (err != nil) != tt.wantErr {
				t.Fatalf("checkDemoKeys() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}
