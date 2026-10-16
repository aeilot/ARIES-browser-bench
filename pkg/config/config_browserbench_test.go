package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const browserbenchBenchmark = `"benchmark":{"type":"browserbench","root":".cache/browserbench","tasks":["` + "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" + `"],"environment":{"image":"mcr.microsoft.com/playwright/mcp:v0.0.83","workdir":"/home/node"}}`

const browserbenchVersions = `,"browserbench":{"assistantbench":{"repository_url":"https://example.invalid/datasets/ab","revision":"4444444444444444444444444444444444444444","dev_file":"dev.jsonl","dev_sha256":"` + "c1abd710b99d6f810e2ce95cb6c43f31828dacd778c9e4d9dd4ddad2eaee6ac4" + `"}}`

func browserbenchConfig() string {
	profile := strings.Replace(validConfig, `"benchmark":{"type":"terminalbench2","root":".cache/tb2","tasks":["fix-git"]}`, browserbenchBenchmark, 1)
	profile = strings.Replace(profile, `"harness":{"type":"openclaw"}`, `"harness":{"type":"hermes"}`, 1)
	return strings.Replace(profile, `"bridge":{"type":"openclaw-ssh"}`, `"bridge":{"type":"hermes-ssh"}`, 1)
}

func TestBrowserBenchBenchmarkValidation(t *testing.T) {
	valid := browserbenchConfig()
	cfg, err := Decode(strings.NewReader(valid))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Benchmark.BrowserBench != nil {
		t.Fatalf("browserbench block = %#v, want absent by default", cfg.Benchmark.BrowserBench)
	}
	tuned := strings.Replace(valid, `"environment":`, `"browserbench":{"mcp_port":9000,"mcp_call_timeout_seconds":60,"blocked_origins":["https://github.com","huggingface.co"]},"environment":`, 1)
	cfg, err = Decode(strings.NewReader(tuned))
	if err != nil {
		t.Fatal(err)
	}
	if settings := cfg.Benchmark.BrowserBench; settings == nil || settings.MCPPort != 9000 || settings.MCPCallTimeoutSeconds != 60 || len(settings.BlockedOrigins) != 2 {
		t.Fatalf("browserbench block = %#v", cfg.Benchmark.BrowserBench)
	}

	for name, testCase := range map[string]struct{ from, to, want string }{
		"missing image":  {`"environment":{"image":"mcr.microsoft.com/playwright/mcp:v0.0.83","workdir":"/home/node"}`, `"environment":{"workdir":"/home/node"}`, "environment.image"},
		"judge":          {`"environment":`, `"judge":{"provider":"deepseek","base_url":"https://api.deepseek.com","id":"deepseek-flash","api_key_env":"DEEPSEEK_API_KEY"},"environment":`, "judge"},
		"toolathlon":     {`"environment":`, `"toolathlon":{},"environment":`, "toolathlon"},
		"low port":       {`"environment":`, `"browserbench":{"mcp_port":80},"environment":`, "mcp_port"},
		"bad timeout":    {`"environment":`, `"browserbench":{"mcp_call_timeout_seconds":-1},"environment":`, "mcp_call_timeout_seconds"},
		"origin list":    {`"environment":`, `"browserbench":{"blocked_origins":["a.com;b.com"]},"environment":`, "blocked_origins"},
		"empty origin":   {`"environment":`, `"browserbench":{"blocked_origins":[" "]},"environment":`, "blocked_origins"},
		"other type use": {browserbenchBenchmark, `"benchmark":{"type":"terminalbench2","root":".cache/tb2","tasks":["fix-git"],"browserbench":{}}`, "browserbench must not be set"},
	} {
		t.Run(name, func(t *testing.T) {
			profile := strings.Replace(valid, testCase.from, testCase.to, 1)
			if profile == valid {
				t.Fatal("test case did not change the profile")
			}
			if _, err := Decode(strings.NewReader(profile)); err == nil || !strings.Contains(err.Error(), testCase.want) {
				t.Fatalf("err = %v, want %q", err, testCase.want)
			}
		})
	}
}

func TestBrowserBenchVersionPinRequiredOnlyWhenSelected(t *testing.T) {
	withPin := strings.Replace(validVersions, `,"openclaw":`, browserbenchVersions+`,"openclaw":`, 1)
	if _, err := DecodeVersions(strings.NewReader(withPin)); err != nil {
		t.Fatalf("catalog with browserbench: %v", err)
	}
	for name, broken := range map[string]string{
		"revision": strings.Replace(withPin, "4444444444444444444444444444444444444444", "main", 1),
		"file":     strings.Replace(withPin, `"dev_file":"dev.jsonl"`, `"dev_file":"../dev.jsonl"`, 1),
		"digest":   strings.Replace(withPin, "c1abd710", "C1ABD710", 1),
	} {
		if _, err := DecodeVersions(strings.NewReader(broken)); err == nil || !strings.Contains(err.Error(), "browserbench") {
			t.Fatalf("%s: err = %v", name, err)
		}
	}

	root := t.TempDir()
	profiles := filepath.Join(root, "profiles")
	configs := filepath.Join(root, "configs")
	for _, dir := range []string{profiles, configs} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	profilePath := filepath.Join(profiles, "profile.json")
	if err := os.WriteFile(profilePath, []byte(browserbenchConfig()), 0o600); err != nil {
		t.Fatal(err)
	}
	versionsPath := filepath.Join(configs, "versions.json")
	if err := os.WriteFile(versionsPath, []byte(validVersions), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(profilePath); err == nil || !strings.Contains(err.Error(), "browserbench") {
		t.Fatalf("browserbench profile without a pin: err=%v", err)
	}
	if err := os.WriteFile(versionsPath, []byte(withPin), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(profilePath)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Versions.BrowserBench.AssistantBench.DevFile != "dev.jsonl" {
		t.Fatalf("browserbench pin = %#v", cfg.Versions.BrowserBench)
	}
}
