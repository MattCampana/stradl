package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/tawanorg/claude-sync/internal/storage"
)

// Fake credentials: shaped like the real thing, valid for nothing.
const (
	testSAS        = "sv=2024-11-04&si=stradl-policy&sr=c&sig=FAKEsignatureFORtests%2Bonly%3D"
	testAccountKey = "ZmFrZS1hY2NvdW50LWtleS1mb3ItdGVzdHMtb25seQ=="
)

func TestAzureConfigMapsFlags(t *testing.T) {
	t.Run("SAS token with leading ? and default container", func(t *testing.T) {
		cfg, err := azureConfig(azureOptions{Account: "mystradl", SASToken: "  ?" + testSAS + "\n"})
		if err != nil {
			t.Fatalf("azureConfig() = %v", err)
		}
		want := storage.StorageConfig{
			Provider:      storage.ProviderAzure,
			Bucket:        "stradl",
			AzureAccount:  "mystradl",
			AzureSASToken: testSAS,
		}
		if *cfg != want {
			t.Errorf("azureConfig() = %+v, want %+v", *cfg, want)
		}
	})

	t.Run("account key and custom container", func(t *testing.T) {
		cfg, err := azureConfig(azureOptions{Account: "acct123", Container: "my-sessions", AccountKey: testAccountKey})
		if err != nil {
			t.Fatalf("azureConfig() = %v", err)
		}
		if cfg.Provider != storage.ProviderAzure || cfg.Bucket != "my-sessions" || cfg.AzureAccount != "acct123" ||
			cfg.AzureAccountKey != testAccountKey || cfg.AzureSASToken != "" {
			t.Errorf("azureConfig() = %+v", *cfg)
		}
	})

	t.Run("full Blob SAS URL is reduced to its token", func(t *testing.T) {
		cfg, err := azureConfig(azureOptions{Account: "mystradl", SASToken: "https://mystradl.blob.core.windows.net/stradl?" + testSAS})
		if err != nil {
			t.Fatalf("azureConfig() = %v", err)
		}
		if cfg.AzureSASToken != testSAS {
			t.Errorf("AzureSASToken = %q, want %q", cfg.AzureSASToken, testSAS)
		}
	})
}

func TestAzureConfigValidation(t *testing.T) {
	tests := []struct {
		name    string
		opts    azureOptions
		wantErr string
	}{
		{"missing account", azureOptions{SASToken: testSAS}, "--azure-account"},
		{"uppercase account", azureOptions{Account: "MyStradl", SASToken: testSAS}, "3-24 lowercase"},
		{"short account", azureOptions{Account: "ab", SASToken: testSAS}, "3-24 lowercase"},
		{"long account", azureOptions{Account: strings.Repeat("a", 25), SASToken: testSAS}, "3-24 lowercase"},
		{"hyphenated account", azureOptions{Account: "my-stradl", SASToken: testSAS}, "3-24 lowercase"},
		{"bad container", azureOptions{Account: "mystradl", Container: "My_Container", SASToken: testSAS}, "container name"},
		{"double hyphen container", azureOptions{Account: "mystradl", Container: "a--b", SASToken: testSAS}, "container name"},
		{"no credential", azureOptions{Account: "mystradl"}, "credential is required"},
		{"both credentials", azureOptions{Account: "mystradl", SASToken: testSAS, AccountKey: testAccountKey}, "not both"},
		{"SAS without signature", azureOptions{Account: "mystradl", SASToken: "sv=2024-11-04&sr=c"}, "sig="},
		{"account key pasted as SAS", azureOptions{Account: "mystradl", SASToken: testAccountKey}, "sig="},
		{"key not base64", azureOptions{Account: "mystradl", AccountKey: "not base64!!"}, "base64"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := azureConfig(tt.opts)
			if err == nil {
				t.Fatalf("azureConfig(%s) expected error", tt.name)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error = %q, want it to contain %q", err, tt.wantErr)
			}
			for _, secret := range []string{tt.opts.SASToken, tt.opts.AccountKey} {
				if len(secret) > 8 && strings.Contains(err.Error(), secret) {
					t.Errorf("error leaks a secret: %q", err)
				}
			}
		})
	}
}

func TestNormalizeSASToken(t *testing.T) {
	tests := map[string]string{
		"?sv=1&sig=abc":     "sv=1&sig=abc",
		"  sv=1&sig=abc \n": "sv=1&sig=abc",
		"sv=1&sig=abc":      "sv=1&sig=abc",
		"https://a.blob.core.windows.net/stradl?sv=1&sig=abc": "sv=1&sig=abc",
		"": "",
	}
	for in, want := range tests {
		if got := normalizeSASToken(in); got != want {
			t.Errorf("normalizeSASToken(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRunAzureWizardNonInteractive(t *testing.T) {
	t.Run("complete flags need no prompts", func(t *testing.T) {
		cfg, err := runAzureWizard(azureOptions{Account: "mystradl", SASToken: testSAS}, false)
		if err != nil {
			t.Fatalf("runAzureWizard() = %v", err)
		}
		if cfg.Bucket != defaultAzureContainer || cfg.AzureSASToken != testSAS {
			t.Errorf("runAzureWizard() = %+v", *cfg)
		}
	})

	t.Run("complete flags skip prompts even on a terminal", func(t *testing.T) {
		cfg, err := runAzureWizard(azureOptions{Account: "mystradl", Container: "team", AccountKey: testAccountKey}, true)
		if err != nil {
			t.Fatalf("runAzureWizard() = %v", err)
		}
		if cfg.Bucket != "team" || cfg.AzureAccountKey != testAccountKey {
			t.Errorf("runAzureWizard() = %+v", *cfg)
		}
	})

	t.Run("missing secret without a terminal explains the flags", func(t *testing.T) {
		_, err := runAzureWizard(azureOptions{Account: "mystradl"}, false)
		if err == nil || !strings.Contains(err.Error(), "--azure-sas-token") || !strings.Contains(err.Error(), "not a terminal") {
			t.Fatalf("runAzureWizard() error = %v", err)
		}
	})
}

func TestResolveProvider(t *testing.T) {
	if got, err := resolveProvider(initOptions{provider: "s3"}, false); err != nil || got != "s3" {
		t.Errorf("explicit --provider: got %q, %v", got, err)
	}
	if got, err := resolveProvider(initOptions{azure: azureOptions{Account: "mystradl"}}, false); err != nil || got != "azure" {
		t.Errorf("azure flags imply azure: got %q, %v", got, err)
	}
	if _, err := resolveProvider(initOptions{}, false); err == nil {
		t.Error("no provider and no terminal: expected error")
	}
}

func TestProviderMenuDefaultsToAzure(t *testing.T) {
	if providerChoices[0].id != "azure" || !strings.Contains(providerChoices[0].label, "Azure Blob Storage") {
		t.Errorf("first provider choice = %+v, want Azure", providerChoices[0])
	}
	seen := map[string]bool{}
	for _, c := range providerChoices {
		if seen[c.id] {
			t.Errorf("duplicate provider %q", c.id)
		}
		seen[c.id] = true
	}
	for _, id := range []string{"azure", "r2", "s3", "gcs", "s3-compatible", "webdav"} {
		if !seen[id] {
			t.Errorf("provider menu is missing %q", id)
		}
	}
}

func TestRedactSAS(t *testing.T) {
	msg := `Get "https://typo.blob.core.windows.net/stradl?comp=list&restype=container&sig=SECRET%2Bvalue&sv=2024": dial tcp: no such host`
	got := redactSAS(msg)
	if strings.Contains(got, "SECRET") {
		t.Errorf("redactSAS() left the signature: %q", got)
	}
	if !strings.Contains(got, "sig=REDACTED&sv=2024") {
		t.Errorf("redactSAS() = %q", got)
	}
}

func TestAzureConnectionMessages(t *testing.T) {
	sasCfg := &storage.StorageConfig{Provider: storage.ProviderAzure, Bucket: "stradl", AzureAccount: "mystradl", AzureSASToken: testSAS}
	keyCfg := &storage.StorageConfig{Provider: storage.ProviderAzure, Bucket: "stradl", AzureAccount: "mystradl", AzureAccountKey: testAccountKey}

	if got := connectedMessage(sasCfg); got != "Connected to Azure container 'stradl' in account 'mystradl'" {
		t.Errorf("connectedMessage() = %q", got)
	}

	err := bucketCheckError(sasCfg, errors.New("GET https://mystradl.blob.core.windows.net/stradl?sig=LEAKED: 403"))
	for _, want := range []string{"'stradl'", "'mystradl'", "expired", "List permission"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("bucketCheckError(SAS) = %q, want %q", err, want)
		}
	}
	if strings.Contains(err.Error(), "LEAKED") {
		t.Errorf("bucketCheckError leaks the SAS signature: %q", err)
	}

	if err := bucketCheckError(keyCfg, errors.New("403")); !strings.Contains(err.Error(), "Access keys") {
		t.Errorf("bucketCheckError(key) = %q, want access key hint", err)
	}
	if err := missingBucketError(sasCfg); !strings.Contains(err.Error(), "+ Container") {
		t.Errorf("missingBucketError() = %q", err)
	}

	s3Cfg := &storage.StorageConfig{Provider: storage.ProviderS3, Bucket: "b"}
	if got := connectedMessage(s3Cfg); got != "Connected to 'b'" {
		t.Errorf("connectedMessage(s3) = %q", got)
	}
}
