package main

import (
	"encoding/base64"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strings"

	"github.com/AlecAivazis/survey/v2"

	"github.com/tawanorg/claude-sync/internal/storage"
)

// defaultAzureContainer is the container Stradl suggests, and uses when
// --bucket is omitted, for Azure Blob Storage.
const defaultAzureContainer = "stradl"

var (
	// azureAccountRe matches storage account names: 3-24 lowercase letters
	// and digits.
	azureAccountRe = regexp.MustCompile(`^[a-z0-9]{3,24}$`)

	// azureContainerRe matches container names: 3-63 lowercase letters,
	// digits and single hyphens, starting and ending with a letter or digit.
	azureContainerRe = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9]|-[a-z0-9]){2,62}$`)

	// sasSigRe finds the signature of a SAS token in any string (an error
	// message quoting a request URL, say), so it can be redacted.
	sasSigRe = regexp.MustCompile(`(?i)(sig=)[^&\s"']+`)
)

// azureOptions holds the Azure values given as init flags. Blank fields are
// prompted for when stdin is a terminal.
type azureOptions struct {
	Account    string // --azure-account
	Container  string // --bucket (default "stradl")
	SASToken   string // --azure-sas-token
	AccountKey string // --azure-account-key
}

// hasAny reports whether any Azure-specific flag was given, which lets init
// infer --provider azure.
func (o azureOptions) hasAny() bool {
	return o.Account != "" || o.SASToken != "" || o.AccountKey != ""
}

// complete reports whether the flags alone are enough to configure Azure, so
// scripted setup never stops at a prompt.
func (o azureOptions) complete() bool {
	return o.Account != "" && (o.SASToken != "" || o.AccountKey != "")
}

func validateAzureAccount(name string) error {
	if name == "" {
		return fmt.Errorf("storage account name is required (--azure-account)")
	}
	if !azureAccountRe.MatchString(name) {
		return fmt.Errorf("storage account name must be 3-24 lowercase letters and digits")
	}
	return nil
}

func validateAzureContainer(name string) error {
	if !azureContainerRe.MatchString(name) {
		return fmt.Errorf("container name must be 3-63 lowercase letters, digits and single hyphens")
	}
	return nil
}

// normalizeSASToken trims whitespace and a leading "?" from a SAS token. When
// a full "Blob SAS URL" was pasted instead, only its query string is kept.
func normalizeSASToken(s string) string {
	s = strings.TrimSpace(s)
	if strings.Contains(s, "://") {
		if u, err := url.Parse(s); err == nil {
			s = u.RawQuery
		}
	}
	return strings.TrimPrefix(s, "?")
}

// validateSASToken checks that tok (already normalized) looks like a SAS
// token. Errors never quote the token.
func validateSASToken(tok string) error {
	if tok == "" {
		return fmt.Errorf("SAS token is required")
	}
	q, err := url.ParseQuery(tok)
	if err != nil || q.Get("sig") == "" {
		return fmt.Errorf("that doesn't look like a SAS token (no sig= signature); copy the whole \"Blob SAS token\" value from the portal")
	}
	return nil
}

// validateAccountKey checks that key is base64, as Azure shows it under
// Security + networking -> Access keys. Errors never quote the key.
func validateAccountKey(key string) error {
	if key == "" {
		return fmt.Errorf("account key is required")
	}
	if _, err := base64.StdEncoding.DecodeString(key); err != nil {
		return fmt.Errorf("account key must be the base64 value shown under Security + networking → Access keys")
	}
	return nil
}

// azureConfig validates opts and maps them to a storage config. The container
// defaults to "stradl"; exactly one credential must be set.
func azureConfig(opts azureOptions) (*storage.StorageConfig, error) {
	account := strings.TrimSpace(opts.Account)
	container := strings.TrimSpace(opts.Container)
	if container == "" {
		container = defaultAzureContainer
	}
	sas := normalizeSASToken(opts.SASToken)
	key := strings.TrimSpace(opts.AccountKey)

	if err := validateAzureAccount(account); err != nil {
		return nil, err
	}
	if err := validateAzureContainer(container); err != nil {
		return nil, err
	}
	switch {
	case sas != "" && key != "":
		return nil, fmt.Errorf("use either --azure-sas-token or --azure-account-key, not both")
	case sas != "":
		if err := validateSASToken(sas); err != nil {
			return nil, err
		}
	case key != "":
		if err := validateAccountKey(key); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("an Azure credential is required: --azure-sas-token (recommended) or --azure-account-key")
	}

	cfg := &storage.StorageConfig{
		Provider:        storage.ProviderAzure,
		Bucket:          container,
		AzureAccount:    account,
		AzureSASToken:   sas,
		AzureAccountKey: key,
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// stdinIsTerminal reports whether stdin is an interactive terminal, i.e.
// whether init may prompt.
func stdinIsTerminal() bool {
	fi, err := os.Stdin.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

// runAzureWizard configures Azure Blob Storage. When the flags already name
// the account and a credential (scripted setup), or stdin is not a terminal,
// it uses opts as given; otherwise it prompts for whatever is missing, reading
// the secret with a hidden prompt so it never lands in shell history.
func runAzureWizard(opts azureOptions, interactive bool) (*storage.StorageConfig, error) {
	fmt.Printf("  %sAzure Blob Storage Setup%s\n\n", colorBold, colorReset)

	if opts.complete() || !interactive {
		cfg, err := azureConfig(opts)
		if err != nil && !interactive {
			return nil, fmt.Errorf("%w\n  (stdin is not a terminal, so init can't prompt; pass --azure-account and --azure-sas-token)", err)
		}
		return cfg, err
	}

	printInfo("Stradl keeps your encrypted sessions in a private container in your own")
	printInfo("storage account. You need the account, a container, and a SAS token.")
	fmt.Println()
	fmt.Printf("  %s1.%s Storage account: %shttps://portal.azure.com/#create/Microsoft.StorageAccount%s\n",
		colorCyan, colorReset, colorDim, colorReset)
	printInfo("   Standard performance and LRS redundancy are plenty")
	fmt.Printf("  %s2.%s Container: Storage account → Data storage → Containers → %s+ Container%s\n",
		colorCyan, colorReset, colorDim, colorReset)
	printInfo("   Name it \"" + defaultAzureContainer + "\" and keep access private")
	fmt.Printf("  %s3.%s SAS token: open the container → %sShared access tokens%s\n",
		colorCyan, colorReset, colorDim, colorReset)
	printInfo("   Permissions: Read, Add, Create, Write, Delete, List; pick a distant expiry")
	printInfo("   Tip: sign it with a stored access policy so you can revoke it later")
	fmt.Println()

	account := strings.TrimSpace(opts.Account)
	if account == "" {
		prompt := &survey.Input{
			Message: "Storage account name:",
			Help:    "The account name only, e.g. 'mystradl' from mystradl.blob.core.windows.net",
		}
		if err := survey.AskOne(prompt, &account, survey.WithValidator(func(ans interface{}) error {
			return validateAzureAccount(strings.TrimSpace(ans.(string)))
		})); err != nil {
			return nil, err
		}
		account = strings.TrimSpace(account)
	}

	container := strings.TrimSpace(opts.Container)
	if container == "" {
		prompt := &survey.Input{
			Message: "Container name:",
			Default: defaultAzureContainer,
		}
		if err := survey.AskOne(prompt, &container, survey.WithValidator(func(ans interface{}) error {
			return validateAzureContainer(strings.TrimSpace(ans.(string)))
		})); err != nil {
			return nil, err
		}
		container = strings.TrimSpace(container)
	}

	result := azureOptions{Account: account, Container: container, SASToken: opts.SASToken, AccountKey: opts.AccountKey}
	if result.SASToken == "" && result.AccountKey == "" {
		authPrompt := &survey.Select{
			Message: "Authenticate with:",
			Options: []string{
				"SAS token (recommended: scoped to one container, revocable)",
				"Account key (full access to every container in the account)",
			},
		}
		var auth int
		if err := survey.AskOne(authPrompt, &auth); err != nil {
			return nil, err
		}

		var secret string
		if auth == 0 {
			prompt := &survey.Password{
				Message: "SAS token (input hidden):",
				Help:    "Paste the \"Blob SAS token\" (or the whole Blob SAS URL) from the container's Shared access tokens page",
			}
			if err := survey.AskOne(prompt, &secret, survey.WithValidator(func(ans interface{}) error {
				return validateSASToken(normalizeSASToken(ans.(string)))
			})); err != nil {
				return nil, err
			}
			result.SASToken = secret
		} else {
			prompt := &survey.Password{
				Message: "Account key (input hidden):",
				Help:    "Storage account → Security + networking → Access keys → key1",
			}
			if err := survey.AskOne(prompt, &secret, survey.WithValidator(func(ans interface{}) error {
				return validateAccountKey(strings.TrimSpace(ans.(string)))
			})); err != nil {
				return nil, err
			}
			result.AccountKey = secret
		}
	}

	return azureConfig(result)
}

// redactSAS replaces SAS signatures in s, so errors that quote a request URL
// (a DNS failure for a mistyped account, say) never print the secret.
func redactSAS(s string) string {
	return sasSigRe.ReplaceAllString(s, "${1}REDACTED")
}

// connectedMessage is init's success line once the bucket check passes.
func connectedMessage(cfg *storage.StorageConfig) string {
	switch cfg.Provider {
	case storage.ProviderWebDAV:
		return "Connected to WebDAV ('" + cfg.PathPrefix + "')"
	case storage.ProviderAzure:
		return fmt.Sprintf("Connected to Azure container '%s' in account '%s'", cfg.Bucket, cfg.AzureAccount)
	default:
		return "Connected to '" + cfg.Bucket + "'"
	}
}

// bucketCheckError explains a failed bucket check, with a provider-specific
// hint where one helps.
func bucketCheckError(cfg *storage.StorageConfig, err error) error {
	if cfg.Provider != storage.ProviderAzure {
		return fmt.Errorf("could not verify bucket '%s': %w", cfg.Bucket, err)
	}
	hint := "check the account key (Storage account → Security + networking → Access keys)"
	if cfg.AzureSASToken != "" {
		hint = "the SAS token may have expired, or it is missing List permission (it needs Read, Add, Create, Write, Delete and List)"
	}
	return fmt.Errorf("could not reach Azure container '%s' in account '%s': %s\n  Hint: %s",
		cfg.Bucket, cfg.AzureAccount, redactSAS(err.Error()), hint)
}

// missingBucketError explains that the configured bucket does not exist.
func missingBucketError(cfg *storage.StorageConfig) error {
	switch cfg.Provider {
	case storage.ProviderWebDAV:
		return fmt.Errorf("could not access WebDAV path '%s' - check your URL and credentials", cfg.PathPrefix)
	case storage.ProviderAzure:
		return fmt.Errorf("container '%s' does not exist in storage account '%s'.\n"+
			"  Create it in the Azure portal: Storage account → Data storage → Containers → + Container (private access)",
			cfg.Bucket, cfg.AzureAccount)
	default:
		return fmt.Errorf("bucket '%s' does not exist. Please create it first in your storage provider's console.\n  For R2: https://dash.cloudflare.com/ → R2 → Create bucket\n  For S3: https://console.aws.amazon.com/s3/ → Create bucket (use 'automatic' location)\n  For GCS: https://console.cloud.google.com/storage/ → Create bucket", cfg.Bucket)
	}
}
