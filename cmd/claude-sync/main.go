package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/AlecAivazis/survey/v2"
	"github.com/spf13/cobra"

	"github.com/tawanorg/claude-sync/internal/claudesettings"
	"github.com/tawanorg/claude-sync/internal/config"
	"github.com/tawanorg/claude-sync/internal/crypto"
	"github.com/tawanorg/claude-sync/internal/paths"
	"github.com/tawanorg/claude-sync/internal/storage"
	"github.com/tawanorg/claude-sync/internal/sync"
	"github.com/tawanorg/claude-sync/internal/util"

	// Register storage adapters
	_ "github.com/tawanorg/claude-sync/internal/storage/azure"
	_ "github.com/tawanorg/claude-sync/internal/storage/gcs"
	_ "github.com/tawanorg/claude-sync/internal/storage/r2"
	_ "github.com/tawanorg/claude-sync/internal/storage/s3"
	_ "github.com/tawanorg/claude-sync/internal/storage/webdav"
)

var (
	version = "dev" // Set via ldflags at build time: -ldflags "-X main.version=x.x.x"
	quiet   bool
)

// extraCommands lets commands defined in other files of this package register
// themselves from init() without editing the list in main.
var extraCommands []func() *cobra.Command

// ANSI color codes
const (
	colorReset  = "\033[0m"
	colorBold   = "\033[1m"
	colorDim    = "\033[2m"
	colorCyan   = "\033[36m"
	colorGreen  = "\033[32m"
	colorYellow = "\033[33m"
)

// tagline is Stradl's one-line pitch, shown under the banner.
const tagline = "Musical chairs for your sessions."

// upstreamAttribution credits the project Stradl is built on.
const upstreamAttribution = "Built on tawanorg/claude-sync (MIT)"

func main() {
	rootCmd := &cobra.Command{
		Use:   "stradl",
		Short: "Musical chairs for your Claude Code sessions",
		Long: `Stradl keeps your Claude Code sessions in sync across all of your Macs, even
when each one has a different username. Get up from one Mac, sit down at
another, and pick up the same conversation.

Sessions are compressed and encrypted on your Mac with age before they are
uploaded to cloud storage you own (Azure Blob Storage, Cloudflare R2, Amazon
S3, Google Cloud Storage, any S3-compatible service, or WebDAV), so your
storage provider only ever sees ciphertext.

Get started:
  stradl init             connect your storage and set up encryption
  stradl sync             two-way sync with your other Macs
  stradl agent install    keep this Mac in sync in the background

` + upstreamAttribution + `.`,
		Version: version,
		// Flags parsed fine by the time this runs, so a later error is a
		// runtime failure: print it without the usage text.
		PersistentPreRun: func(cmd *cobra.Command, args []string) {
			cmd.SilenceUsage = true
		},
	}

	rootCmd.PersistentFlags().BoolVarP(&quiet, "quiet", "q", false, "Suppress output")

	rootCmd.AddCommand(
		initCmd(),
		pushCmd(),
		pullCmd(),
		statusCmd(),
		diffCmd(),
		conflictsCmd(),
		rebuildHistoryCmd(),
		resetCmd(),
		migrateCmd(),
		updateCmd(),
		changelogCmd(),
		mcpCmd(),
		autoCmd(),
		pathsCmd(),
	)
	for _, mk := range extraCommands {
		rootCmd.AddCommand(mk())
	}

	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}

// stradlLogo is the STRADL wordmark in block letters. Every row is the same
// width so the per-row gradient lines up.
var stradlLogo = []string{
	"███████╗████████╗██████╗  █████╗ ██████╗ ██╗     ",
	"██╔════╝╚══██╔══╝██╔══██╗██╔══██╗██╔══██╗██║     ",
	"███████╗   ██║   ██████╔╝███████║██║  ██║██║     ",
	"╚════██║   ██║   ██╔══██╗██╔══██║██║  ██║██║     ",
	"███████║   ██║   ██║  ██║██║  ██║██████╔╝███████╗",
	"╚══════╝   ╚═╝   ╚═╝  ╚═╝╚═╝  ╚═╝╚═════╝ ╚══════╝",
}

// logoGradient colors the logo rows top to bottom, cyan into blue (xterm-256
// palette indexes).
var logoGradient = []int{51, 45, 39, 33, 27, 26}

func printBanner() {
	fmt.Println()
	shownVersion := "v" + strings.TrimPrefix(version, "v")
	if version == "dev" {
		shownVersion = "dev build"
	}
	fmt.Printf("  %sWelcome to Stradl%s %s%s%s\n", colorBold, colorReset, colorDim, shownVersion, colorReset)
	fmt.Println()

	for i, row := range stradlLogo {
		fmt.Printf("  \033[38;5;%dm%s%s\n", logoGradient[i%len(logoGradient)], row, colorReset)
	}
	fmt.Println()

	fmt.Printf("  %s%s%s\n", colorBold, tagline, colorReset)
	fmt.Printf("  %sKeep Claude Code sessions in sync across all your Macs, encrypted, in storage you own.%s\n", colorDim, colorReset)
	if url := projectURL(); url != "" {
		fmt.Printf("  %sIssues & PRs welcome: %s%s%s\n", colorDim, colorCyan, url, colorReset)
	}
	fmt.Printf("  %s%s%s\n", colorDim, upstreamAttribution, colorReset)
	fmt.Println()
}

func printStep(step int, total int, text string) {
	fmt.Printf("\n%s[%d/%d]%s %s%s%s\n", colorCyan, step, total, colorReset, colorBold, text, colorReset)
}

func printInfo(text string) {
	fmt.Printf("      %s%s%s\n", colorDim, text, colorReset)
}

func printSuccess(text string) {
	fmt.Printf("  %s%s%s\n", colorGreen, text, colorReset)
}

func printWarning(text string) {
	fmt.Printf("  %s%s%s\n", colorYellow, text, colorReset)
}

// initOptions collects the init flags.
type initOptions struct {
	provider, bucket, scope string
	usePassphrase, force    bool

	// R2 / S3 / S3-compatible
	accountID, accessKey, secretKey string
	s3Region, s3Endpoint            string
	s3UsePathStyle                  bool

	// GCS
	gcsProjectID, gcsCredentialsFile string

	// WebDAV
	webdavURL, webdavUsername, webdavPassword, webdavPathPrefix string

	// Azure (the container comes from --bucket)
	azure azureOptions
}

// providerChoice is one entry of the init provider menu.
type providerChoice struct {
	id    string
	label string
}

// providerChoices is the init provider menu, default first.
var providerChoices = []providerChoice{
	{"azure", "Azure Blob Storage (your own Azure subscription)"},
	{"r2", "Cloudflare R2 (free tier: 10GB)"},
	{"s3", "Amazon S3"},
	{"gcs", "Google Cloud Storage"},
	{"s3-compatible", "S3-compatible (custom endpoint) — Backblaze B2, MinIO, Wasabi, ..."},
	{"webdav", "WebDAV (Nextcloud, ownCloud, etc. - self-hosted)"},
}

func initCmd() *cobra.Command {
	var opts initOptions

	cmd := &cobra.Command{
		Use:   "init",
		Short: "Connect your cloud storage and set up encryption",
		Long: `Set up cloud storage credentials and generate encryption keys.

Supported providers:
  - azure:         Azure Blob Storage in your own subscription (default)
  - r2:            Cloudflare R2 (S3-compatible, free tier: 10GB)
  - s3:            Amazon S3
  - gcs:           Google Cloud Storage
  - s3-compatible: Any S3-compatible provider via custom endpoint (Backblaze B2, MinIO, Wasabi, ...)
  - webdav:        WebDAV (Nextcloud, ownCloud, etc. - self-hosted)

New setups sync sessions only (conversation history, plans and tasks); pass
--scope full to also sync settings, plugins, skills and agents.

Examples:
  stradl init                # Full setup wizard
  stradl init --passphrase   # Re-enter passphrase only (keeps storage config)
  stradl init --force        # Reset everything, start fresh
  stradl init --azure-account mystradl   # Azure; prompts for the SAS token (hidden)
  stradl init --provider s3-compatible --endpoint https://s3.us-west-004.backblazeb2.com   # Backblaze B2`,
		RunE: func(cmd *cobra.Command, args []string) error {
			// Show banner
			printBanner()

			ctx := context.Background()
			keyPath := config.AgeKeyFilePath()

			// Special case: --passphrase with existing config = just regenerate key
			if opts.usePassphrase && config.Exists() && !opts.force {
				return initPassphraseOnly(ctx, keyPath)
			}

			// Normal flow: full setup
			return initFullSetup(ctx, keyPath, opts)
		},
	}

	// Provider selection
	cmd.Flags().StringVar(&opts.provider, "provider", "", "Storage provider: azure, r2, s3, gcs, s3-compatible, or webdav")
	cmd.Flags().StringVar(&opts.scope, "scope", "", "Sync scope: 'sessions' (default, conversation history only) or 'full' (also settings, plugins, skills, agents)")
	cmd.Flags().StringVar(&opts.bucket, "bucket", "", "Bucket name (the container for Azure, default \""+defaultAzureContainer+"\")")
	cmd.Flags().BoolVar(&opts.usePassphrase, "passphrase", false, "Derive encryption key from passphrase")
	cmd.Flags().BoolVarP(&opts.force, "force", "f", false, "Overwrite existing config/key without prompting")

	// Azure flags
	cmd.Flags().StringVar(&opts.azure.Account, "azure-account", "", "Azure storage account name")
	cmd.Flags().StringVar(&opts.azure.SASToken, "azure-sas-token", "", "Azure container SAS token (prompted for, hidden, when omitted)")
	cmd.Flags().StringVar(&opts.azure.AccountKey, "azure-account-key", "", "Azure storage account key (instead of a SAS token)")

	// R2 flags
	cmd.Flags().StringVar(&opts.accountID, "account-id", "", "Cloudflare Account ID (R2)")
	cmd.Flags().StringVar(&opts.accessKey, "access-key", "", "Access Key ID (R2/S3)")
	cmd.Flags().StringVar(&opts.secretKey, "secret-key", "", "Secret Access Key (R2/S3)")

	// S3 flags
	cmd.Flags().StringVar(&opts.s3Region, "region", "", "Region (S3 / S3-compatible)")

	// S3-compatible flags
	cmd.Flags().StringVar(&opts.s3Endpoint, "endpoint", "", "Custom S3-compatible endpoint URL (e.g. https://s3.us-west-004.backblazeb2.com)")
	cmd.Flags().BoolVar(&opts.s3UsePathStyle, "use-path-style", false, "Use path-style URLs (endpoint/bucket/key) for S3-compatible endpoints that don't resolve buckets as subdomains (e.g. Ceph, MinIO)")

	// GCS flags
	cmd.Flags().StringVar(&opts.gcsProjectID, "project-id", "", "GCP Project ID (GCS)")
	cmd.Flags().StringVar(&opts.gcsCredentialsFile, "credentials-file", "", "Path to GCS credentials JSON file")

	// WebDAV flags
	cmd.Flags().StringVar(&opts.webdavURL, "webdav-url", "", "WebDAV URL (e.g. https://cloud.example.com/remote.php/dav/files/user/)")
	cmd.Flags().StringVar(&opts.webdavUsername, "webdav-username", "", "WebDAV username")
	cmd.Flags().StringVar(&opts.webdavPassword, "webdav-password", "", "WebDAV app password")
	cmd.Flags().StringVar(&opts.webdavPathPrefix, "webdav-path-prefix", "stradl", "WebDAV path prefix (subdirectory)")

	return cmd
}

// resolveProvider returns the provider to configure: the --provider flag, or
// "azure" when only Azure flags were given, or the user's pick from the menu
// (Azure first and default). Without a terminal it can't ask, so it errors.
func resolveProvider(opts initOptions, interactive bool) (string, error) {
	if opts.provider != "" {
		return opts.provider, nil
	}
	if opts.azure.hasAny() {
		return "azure", nil
	}
	if !interactive {
		return "", fmt.Errorf("--provider is required when stdin is not a terminal")
	}

	labels := make([]string, len(providerChoices))
	for i, c := range providerChoices {
		labels[i] = c.label
	}
	prompt := &survey.Select{
		Message: "Choose your cloud storage provider:",
		Options: labels,
		Default: labels[0],
	}
	var choice int
	if err := survey.AskOne(prompt, &choice); err != nil {
		return "", err
	}
	return providerChoices[choice].id, nil
}

// initPassphraseOnly handles the case where user just wants to re-enter passphrase
// keeping existing storage configuration
func initPassphraseOnly(ctx context.Context, keyPath string) error {
	existingCfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("failed to load existing config: %w", err)
	}

	storageCfg := existingCfg.GetStorageConfig()
	store, err := storage.New(storageCfg)
	if err != nil {
		return fmt.Errorf("failed to connect to storage: %w", err)
	}

	fmt.Printf("  %sUsing existing storage config:%s %s/%s\n\n",
		colorDim, colorReset, storageCfg.Provider, storageCfg.Bucket)

	printInfo("Use the SAME passphrase on all devices.")

	shouldClearRemote, err := enterPassphraseAndVerify(ctx, store, keyPath)
	if err != nil {
		return err
	}

	// Clear remote if user chose to start fresh
	if shouldClearRemote {
		fmt.Printf("%s⋯%s Clearing remote files...\n", colorDim, colorReset)
		if err := clearRemoteStorage(ctx, store); err != nil {
			printWarning("Failed to clear remote: " + err.Error())
		} else {
			printSuccess("Remote files cleared")
		}
	}

	fmt.Println()
	fmt.Println(colorGreen + "  Passphrase updated!" + colorReset)
	fmt.Println()

	return nil
}

// resolveScope validates a --scope value. Returns "full" or "sessions". An
// empty value means "sessions": Stradl syncs portable conversation data, not
// machine-specific settings and plugins, unless asked to with --scope full.
func resolveScope(scope string) (string, error) {
	switch scope {
	case config.ScopeFull, config.ScopeSessions:
		return scope, nil
	case "":
		return config.ScopeSessions, nil
	default:
		return "", fmt.Errorf("invalid --scope %q (use \"sessions\" or \"full\")", scope)
	}
}

// initFullSetup handles the full init wizard
func initFullSetup(ctx context.Context, keyPath string, opts initOptions) error {
	usePassphrase, force := opts.usePassphrase, opts.force

	// Reject a bad --scope before asking for anything.
	scope, err := resolveScope(opts.scope)
	if err != nil {
		return err
	}

	if config.Exists() && !force {
		var overwrite bool
		prompt := &survey.Confirm{
			Message: "Configuration already exists. Overwrite?",
			Default: false,
		}
		if err := survey.AskOne(prompt, &overwrite); err != nil || !overwrite {
			fmt.Println("  Aborted.")
			return nil
		}
		fmt.Println()
	}

	// Step 1: Select provider
	printStep(1, 3, "Select Storage Provider")
	fmt.Println()

	interactive := stdinIsTerminal()
	provider, err := resolveProvider(opts, interactive)
	if err != nil {
		return err
	}

	var storageCfg *storage.StorageConfig
	fmt.Println()

	switch provider {
	case "azure":
		azureOpts := opts.azure
		azureOpts.Container = opts.bucket
		storageCfg, err = runAzureWizard(azureOpts, interactive)
	case "r2":
		storageCfg, err = runR2Wizard(opts.accountID, opts.accessKey, opts.secretKey, opts.bucket)
	case "s3":
		storageCfg, err = runS3Wizard(opts.accessKey, opts.secretKey, opts.s3Region, opts.bucket)
	case "gcs":
		storageCfg, err = runGCSWizard(opts.gcsProjectID, opts.gcsCredentialsFile, opts.bucket)
	case "s3-compatible":
		storageCfg, err = runS3CompatibleWizard(opts.s3Endpoint, opts.accessKey, opts.secretKey, opts.s3Region, opts.bucket, opts.s3UsePathStyle)
	case "webdav":
		storageCfg, err = runWebDAVWizard(opts.webdavURL, opts.webdavUsername, opts.webdavPassword, opts.webdavPathPrefix)
	default:
		return fmt.Errorf("unsupported provider: %s", provider)
	}

	if err != nil {
		return err
	}
	if storageCfg == nil {
		return fmt.Errorf("setup cancelled")
	}

	// Step 2: Encryption setup
	fmt.Println()
	printStep(2, 3, "Set Up Encryption")
	printInfo("Files are encrypted with 'age' before upload.")
	fmt.Println()

	configDir := config.ConfigDirPath()
	if err := os.MkdirAll(configDir, 0700); err != nil {
		return fmt.Errorf("failed to create config directory: %w", err)
	}

	// Check if we should use passphrase mode
	if !usePassphrase && !crypto.KeyExists(keyPath) {
		prompt := &survey.Select{
			Message: "Choose encryption key method:",
			Options: []string{
				"Passphrase (recommended) - same key on all devices",
				"Random key - must copy key file to other devices",
			},
		}
		var choice int
		if err := survey.AskOne(prompt, &choice); err != nil {
			return err
		}
		usePassphrase = choice == 0
		fmt.Println()
	}

	// Create storage client early so we can verify key matches remote
	store, err := storage.New(storageCfg)
	if err != nil {
		if strings.Contains(err.Error(), "could not find default credentials") {
			printWarning("GCS Application Default Credentials not configured.")
			fmt.Println()
			printInfo("To set up ADC, run:")
			fmt.Printf("  %sgcloud auth application-default login%s\n", colorCyan, colorReset)
			fmt.Println()
			printInfo("Or use a service account JSON file instead.")
			return fmt.Errorf("GCS credentials not configured")
		}
		return fmt.Errorf("failed to create storage client: %s", redactSAS(err.Error()))
	}

	shouldClearRemote := false

	if usePassphrase {
		if crypto.KeyExists(keyPath) && !force {
			var overwriteKey bool
			prompt := &survey.Confirm{
				Message: "Encryption key already exists. Overwrite?",
				Default: false,
			}
			if err := survey.AskOne(prompt, &overwriteKey); err != nil || !overwriteKey {
				printSuccess("Using existing key")
				goto skipKeyGen
			}
		}

		printInfo("Use the SAME passphrase on all devices.")
		shouldClearRemote, err = enterPassphraseAndVerify(ctx, store, keyPath)
		if err != nil {
			return err
		}

	} else if !crypto.KeyExists(keyPath) {
		if err := crypto.GenerateKey(keyPath); err != nil {
			return fmt.Errorf("failed to generate key: %w", err)
		}
		printSuccess("Key generated: " + keyPath)
		printWarning("Back up this file! You need it on other devices.")
	} else {
		printSuccess("Using existing key")
	}
skipKeyGen:

	// Step 3: Test Connection
	fmt.Println()
	printStep(3, 3, "Test Connection")

	exists, err := store.BucketExists(ctx)
	if err != nil {
		return bucketCheckError(storageCfg, err)
	} else if !exists {
		return missingBucketError(storageCfg)
	}
	printSuccess(connectedMessage(storageCfg))

	// Clear remote if user chose to start fresh
	if shouldClearRemote {
		fmt.Printf("%s⋯%s Clearing remote files...\n", colorDim, colorReset)
		if err := clearRemoteStorage(ctx, store); err != nil {
			printWarning("Failed to clear remote: " + err.Error())
		} else {
			printSuccess("Remote files cleared")
		}
	}

	// Verify encryption key can decrypt remote files (if any exist)
	if !shouldClearRemote {
		if err := verifyKeyMatchesRemote(ctx, store, keyPath); err != nil {
			fmt.Println()
			printWarning("Encryption key cannot decrypt remote files!")
			printInfo("The remote bucket has files encrypted with a different key.")
			fmt.Println()
			printInfo("Options:")
			printInfo("  1. Run 'stradl init --passphrase' to try a different passphrase")
			printInfo("  2. Copy the age-key.txt from your original device")
			printInfo("  3. Run 'stradl reset --remote' to clear remote and start fresh")
			fmt.Println()
			return fmt.Errorf("encryption key mismatch - cannot sync with remote")
		}
		printSuccess("Encryption key verified")
	}

	// Save config
	cfg := &config.Config{
		Storage:       storageCfg,
		EncryptionKey: "~/" + config.ConfigDir + "/" + config.AgeKeyFile,
		Scope:         scope,
	}

	if err := config.Save(cfg); err != nil {
		return err
	}

	// Done
	fmt.Println()
	fmt.Println(colorGreen + "  Setup complete!" + colorReset)
	if scope == config.ScopeSessions {
		printInfo("Syncing sessions only (use 'stradl init --scope full' to include settings and plugins)")
	}
	fmt.Println()
	printInfo("Next:")
	fmt.Printf("    %sstradl sync%s              sync this Mac now\n", colorCyan, colorReset)
	fmt.Printf("    %sstradl agent install%s     keep it in sync in the background\n", colorCyan, colorReset)
	fmt.Println()
	if usePassphrase {
		printInfo("Then run 'stradl init' with the same passphrase on your other Macs.")
	} else {
		printInfo("Then copy " + keyPath + " to your other Macs and run 'stradl init' there.")
	}
	fmt.Println()

	return nil
}

// enterPassphraseAndVerify prompts for passphrase and verifies against remote
// Returns shouldClearRemote flag
func enterPassphraseAndVerify(ctx context.Context, store storage.Storage, keyPath string) (bool, error) {
	for {
		var passphrase string
		for {
			prompt := &survey.Password{
				Message: "Passphrase (min 8 chars):",
			}
			if err := survey.AskOne(prompt, &passphrase); err != nil {
				return false, err
			}

			if err := crypto.ValidatePassphraseStrength(passphrase); err != nil {
				printWarning(err.Error())
				continue
			}

			var confirm string
			confirmPrompt := &survey.Password{
				Message: "Confirm passphrase:",
			}
			if err := survey.AskOne(confirmPrompt, &confirm); err != nil {
				return false, err
			}

			if passphrase != confirm {
				printWarning("Passphrases don't match.")
				continue
			}
			break
		}

		if err := crypto.GenerateKeyFromPassphrase(keyPath, passphrase); err != nil {
			return false, fmt.Errorf("failed to generate key: %w", err)
		}

		// Verify the key matches existing remote files (if any)
		if err := verifyKeyMatchesRemote(ctx, store, keyPath); err != nil {
			// Key mismatch detected - ask user what to do
			action, actionErr := handleKeyMismatch()
			if actionErr != nil {
				return false, actionErr
			}

			switch action {
			case actionRetryPassphrase:
				_ = os.Remove(keyPath)
				fmt.Println()
				printInfo("Enter a different passphrase:")
				continue
			case actionClearRemote:
				printSuccess("Key derived from passphrase")
				printInfo("Remote files will be cleared...")
				return true, nil
			case actionAbort:
				_ = os.Remove(keyPath)
				return false, fmt.Errorf("setup aborted")
			}
		}

		printSuccess("Key derived from passphrase")
		return false, nil
	}
}

func runR2Wizard(accountID, accessKey, secretKey, bucket string) (*storage.StorageConfig, error) {
	fmt.Printf("  %sCloudflare R2 Setup%s\n\n", colorBold, colorReset)
	printInfo("You need a Cloudflare R2 bucket and API token.")
	printInfo("R2 free tier includes 10GB storage.")
	fmt.Println()
	fmt.Printf("  %s1.%s Create bucket: %shttps://dash.cloudflare.com/?to=/:account/r2/new%s\n",
		colorCyan, colorReset, colorDim, colorReset)
	fmt.Printf("  %s2.%s Create API token: %shttps://dash.cloudflare.com/?to=/:account/r2/api-tokens%s\n",
		colorCyan, colorReset, colorDim, colorReset)
	printInfo("   Select 'Object Read & Write' permission")
	fmt.Println()

	answers := struct {
		AccountID string
		AccessKey string
		SecretKey string
		Bucket    string
	}{
		AccountID: accountID,
		AccessKey: accessKey,
		SecretKey: secretKey,
		Bucket:    bucket,
	}

	questions := []*survey.Question{
		{
			Name: "AccountID",
			Prompt: &survey.Input{
				Message: "Account ID:",
				Help:    "Found in URL: dash.cloudflare.com/<ACCOUNT_ID>/r2",
				Default: accountID,
			},
			Validate: survey.Required,
		},
		{
			Name: "AccessKey",
			Prompt: &survey.Input{
				Message: "Access Key ID:",
				Default: accessKey,
			},
			Validate: survey.Required,
		},
		{
			Name: "SecretKey",
			Prompt: &survey.Password{
				Message: "Secret Access Key:",
			},
			Validate: survey.Required,
		},
		{
			Name: "Bucket",
			Prompt: &survey.Input{
				Message: "Bucket name:",
				Default: "stradl",
			},
			Validate: survey.Required,
		},
	}

	if err := survey.Ask(questions, &answers); err != nil {
		return nil, err
	}

	return &storage.StorageConfig{
		Provider:        storage.ProviderR2,
		Bucket:          answers.Bucket,
		AccountID:       answers.AccountID,
		AccessKeyID:     answers.AccessKey,
		SecretAccessKey: answers.SecretKey,
	}, nil
}

func runS3Wizard(accessKey, secretKey, region, bucket string) (*storage.StorageConfig, error) {
	fmt.Printf("  %sAmazon S3 Setup%s\n\n", colorBold, colorReset)
	printInfo("You need an AWS S3 bucket and IAM credentials.")
	fmt.Println()
	fmt.Printf("  %s1.%s Create bucket: %shttps://s3.console.aws.amazon.com/s3/bucket/create%s\n",
		colorCyan, colorReset, colorDim, colorReset)
	fmt.Printf("  %s2.%s Create access keys: %shttps://console.aws.amazon.com/iam/home#/security_credentials%s\n",
		colorCyan, colorReset, colorDim, colorReset)
	fmt.Println()

	answers := struct {
		AccessKey string
		SecretKey string
		Region    string
		Bucket    string
	}{
		AccessKey: accessKey,
		SecretKey: secretKey,
		Region:    region,
		Bucket:    bucket,
	}

	questions := []*survey.Question{
		{
			Name: "AccessKey",
			Prompt: &survey.Input{
				Message: "Access Key ID:",
				Default: accessKey,
			},
			Validate: survey.Required,
		},
		{
			Name: "SecretKey",
			Prompt: &survey.Password{
				Message: "Secret Access Key:",
			},
			Validate: survey.Required,
		},
		{
			Name: "Region",
			Prompt: &survey.Select{
				Message: "AWS Region:",
				Options: []string{
					"us-east-1",
					"us-east-2",
					"us-west-1",
					"us-west-2",
					"eu-west-1",
					"eu-west-2",
					"eu-central-1",
					"ap-northeast-1",
					"ap-southeast-1",
					"ap-southeast-2",
				},
				Default: "us-east-1",
				Description: func(value string, index int) string {
					descriptions := map[string]string{
						"us-east-1":      "US East (N. Virginia)",
						"us-east-2":      "US East (Ohio)",
						"us-west-1":      "US West (N. California)",
						"us-west-2":      "US West (Oregon)",
						"eu-west-1":      "Europe (Ireland)",
						"eu-west-2":      "Europe (London)",
						"eu-central-1":   "Europe (Frankfurt)",
						"ap-northeast-1": "Asia Pacific (Tokyo)",
						"ap-southeast-1": "Asia Pacific (Singapore)",
						"ap-southeast-2": "Asia Pacific (Sydney)",
					}
					return descriptions[value]
				},
			},
		},
		{
			Name: "Bucket",
			Prompt: &survey.Input{
				Message: "Bucket name:",
				Default: "stradl",
			},
			Validate: survey.Required,
		},
	}

	if err := survey.Ask(questions, &answers); err != nil {
		return nil, err
	}

	return &storage.StorageConfig{
		Provider:        storage.ProviderS3,
		Bucket:          answers.Bucket,
		AccessKeyID:     answers.AccessKey,
		SecretAccessKey: answers.SecretKey,
		Region:          answers.Region,
	}, nil
}

// runS3CompatibleWizard configures any S3-compatible provider (Backblaze B2,
// MinIO, Wasabi, DigitalOcean Spaces, ...) by collecting a custom endpoint URL.
// It reuses the S3 storage engine (ProviderS3 with Endpoint set); the engine
// relaxes checksum behavior for custom endpoints so these providers accept the
// uploads. The signing region is pre-filled from the endpoint when derivable.
func runS3CompatibleWizard(endpoint, accessKey, secretKey, region, bucket string, usePathStyle bool) (*storage.StorageConfig, error) {
	fmt.Printf("  %sS3-Compatible Setup%s\n\n", colorBold, colorReset)
	printInfo("Works with any S3-compatible provider: Backblaze B2, MinIO, Wasabi, DigitalOcean Spaces, ...")
	fmt.Println()
	printInfo("You need the provider's S3 endpoint URL, an access key/secret, and a bucket.")
	fmt.Println()

	// Ask the endpoint first so the signing region can be derived from it.
	if endpoint == "" {
		q := &survey.Input{
			Message: "S3 endpoint URL:",
			Help:    "e.g. https://s3.us-west-004.backblazeb2.com (Backblaze B2)",
		}
		if err := survey.AskOne(q, &endpoint, survey.WithValidator(survey.Required)); err != nil {
			return nil, err
		}
	}

	if region == "" {
		region = storage.RegionFromEndpoint(endpoint)
	}

	answers := struct {
		AccessKey    string
		SecretKey    string
		Region       string
		Bucket       string
		UsePathStyle bool
	}{
		AccessKey:    accessKey,
		SecretKey:    secretKey,
		Region:       region,
		Bucket:       bucket,
		UsePathStyle: usePathStyle,
	}

	questions := []*survey.Question{
		{
			Name: "AccessKey",
			Prompt: &survey.Input{
				Message: "Access Key ID:",
				Default: accessKey,
			},
			Validate: survey.Required,
		},
		{
			Name: "SecretKey",
			Prompt: &survey.Password{
				Message: "Secret Access Key:",
			},
			Validate: survey.Required,
		},
		{
			Name: "Region",
			Prompt: &survey.Input{
				Message: "Region:",
				Default: region,
				Help:    "Signing region, auto-detected from the endpoint. 'auto' works for providers that ignore it.",
			},
			Validate: survey.Required,
		},
		{
			Name: "Bucket",
			Prompt: &survey.Input{
				Message: "Bucket name:",
				Default: "stradl",
			},
			Validate: survey.Required,
		},
		{
			Name: "UsePathStyle",
			Prompt: &survey.Confirm{
				Message: "Use path-style URLs (endpoint/bucket/key)?",
				Default: usePathStyle,
				Help:    "Enable for servers that don't resolve buckets as subdomains, e.g. Ceph RGW or MinIO without wildcard DNS. Leave off for Backblaze B2, Wasabi, DigitalOcean Spaces.",
			},
		},
	}

	if err := survey.Ask(questions, &answers); err != nil {
		return nil, err
	}

	return &storage.StorageConfig{
		Provider:        storage.ProviderS3,
		Bucket:          answers.Bucket,
		AccessKeyID:     answers.AccessKey,
		SecretAccessKey: answers.SecretKey,
		Region:          answers.Region,
		Endpoint:        storage.NormalizeEndpoint(endpoint),
		UsePathStyle:    answers.UsePathStyle,
	}, nil
}

func runGCSWizard(projectID, credentialsFile, bucket string) (*storage.StorageConfig, error) {
	fmt.Printf("  %sGoogle Cloud Storage Setup%s\n\n", colorBold, colorReset)
	printInfo("You need a GCS bucket and service account credentials.")
	fmt.Println()
	fmt.Printf("  %s1.%s Create bucket: %shttps://console.cloud.google.com/storage/create-bucket%s\n",
		colorCyan, colorReset, colorDim, colorReset)
	fmt.Printf("  %s2.%s Create service account: %shttps://console.cloud.google.com/iam-admin/serviceaccounts%s\n",
		colorCyan, colorReset, colorDim, colorReset)
	printInfo("   Grant 'Storage Object Admin' role to the service account")
	fmt.Println()

	answers := struct {
		ProjectID  string
		AuthMethod string
		Bucket     string
	}{
		ProjectID: projectID,
		Bucket:    bucket,
	}

	questions := []*survey.Question{
		{
			Name: "ProjectID",
			Prompt: &survey.Input{
				Message: "GCP Project ID:",
				Default: projectID,
			},
			Validate: survey.Required,
		},
		{
			Name: "AuthMethod",
			Prompt: &survey.Select{
				Message: "Authentication method:",
				Options: []string{
					"Application Default Credentials (requires: gcloud auth application-default login)",
					"Service Account JSON file",
				},
			},
		},
		{
			Name: "Bucket",
			Prompt: &survey.Input{
				Message: "Bucket name:",
				Default: "stradl",
			},
			Validate: survey.Required,
		},
	}

	if err := survey.Ask(questions, &answers); err != nil {
		return nil, err
	}

	cfg := &storage.StorageConfig{
		Provider:  storage.ProviderGCS,
		Bucket:    answers.Bucket,
		ProjectID: answers.ProjectID,
	}

	if strings.Contains(answers.AuthMethod, "Service Account") {
		var credPath string
		prompt := &survey.Input{
			Message: "Credentials JSON file path:",
			Help:    "Path to your service account JSON key file",
			Suggest: func(toComplete string) []string {
				files, _ := filepath.Glob(toComplete + "*")
				return files
			},
		}
		if err := survey.AskOne(prompt, &credPath, survey.WithValidator(func(ans interface{}) error {
			path := ans.(string)
			if path == "" {
				return fmt.Errorf("credentials file path is required")
			}
			// Expand ~ in path
			if len(path) > 0 && path[0] == '~' {
				home, _ := os.UserHomeDir()
				path = home + path[1:]
			}
			if _, err := os.Stat(path); os.IsNotExist(err) {
				return fmt.Errorf("file does not exist: %s", path)
			}
			return nil
		})); err != nil {
			return nil, err
		}
		cfg.CredentialsFile = credPath
	} else {
		cfg.UseDefaultCredentials = true
	}

	return cfg, nil
}

func runWebDAVWizard(webdavURL, username, password, pathPrefix string) (*storage.StorageConfig, error) {
	fmt.Printf("  %sWebDAV Setup (Nextcloud, ownCloud, etc.)%s\n\n", colorBold, colorReset)
	printInfo("You need a WebDAV server URL, username, and an app password.")
	fmt.Println()
	fmt.Printf("  %sNextcloud:%s Go to Settings → Security → Devices & sessions\n", colorCyan, colorReset)
	printInfo("  to create an app password (recommended over your account password)")
	fmt.Println()
	fmt.Printf("  %sURL format:%s https://cloud.example.com/remote.php/dav/files/USERNAME/\n", colorCyan, colorReset)
	fmt.Println()

	answers := struct {
		URL        string
		Username   string
		Password   string
		PathPrefix string
	}{
		URL:        webdavURL,
		Username:   username,
		Password:   password,
		PathPrefix: pathPrefix,
	}

	questions := []*survey.Question{
		{
			Name: "URL",
			Prompt: &survey.Input{
				Message: "WebDAV URL:",
				Default: webdavURL,
				Help:    "For Nextcloud: https://your-server/remote.php/dav/files/USERNAME/",
			},
			Validate: survey.Required,
		},
		{
			Name: "Username",
			Prompt: &survey.Input{
				Message: "Username:",
				Default: username,
			},
			Validate: survey.Required,
		},
		{
			Name: "Password",
			Prompt: &survey.Password{
				Message: "App password:",
				Help:    "Use an app-specific password, not your account password",
			},
		},
		{
			Name: "PathPrefix",
			Prompt: &survey.Input{
				Message: "Path prefix (subdirectory for sync data):",
				Default: func() string {
					if pathPrefix != "" {
						return pathPrefix
					}
					return "stradl"
				}(),
			},
			Validate: survey.Required,
		},
	}

	if err := survey.Ask(questions, &answers); err != nil {
		return nil, err
	}

	if answers.Password == "" && password != "" {
		answers.Password = password
	}
	if answers.Password == "" {
		return nil, fmt.Errorf("app password is required")
	}

	cfg := &storage.StorageConfig{
		Provider:       storage.ProviderWebDAV,
		Bucket:         answers.PathPrefix,
		WebDAVURL:      answers.URL,
		WebDAVUsername: answers.Username,
		WebDAVPassword: answers.Password,
		PathPrefix:     answers.PathPrefix,
	}

	return cfg, nil
}

// twoWaySyncNote is printed by push and pull. They keep their names for muscle
// memory, but the legacy one-way transfers (push propagating local deletes to
// remote, pull trusting device clocks) are gone.
const twoWaySyncNote = "push/pull run a full two-way sync in Stradl"

// runTwoWaySync runs one safe two-way reconcile, waiting for a sync already in
// progress to finish. push and pull reach the engine only through this
// variable, so tests can confirm they never take the legacy code paths.
var runTwoWaySync = func(ctx context.Context, cfg *config.Config) (*sync.SyncReport, error) {
	return sync.RunSync(ctx, cfg, quiet, true)
}

// runMCPSync pushes (push=true) or pulls MCP server configs under the device
// sync lock. A variable so tests can observe it.
var runMCPSync = runMCPLocked

// syncLockPath is the device-wide sync lock for cfg, matching sync.RunSync.
func syncLockPath(cfg *config.Config) string {
	if cfg.StateDirOverride != "" {
		return filepath.Join(cfg.StateDirOverride, config.LockFile)
	}
	return config.LockFilePath()
}

// runMCPLocked runs the MCP push or pull while holding the sync lock. The MCP
// baseline lives in state.json, which a background sync rewrites, so running
// unlocked could lose one side's update.
func runMCPLocked(ctx context.Context, cfg *config.Config, push bool) error {
	release, err := sync.AcquireLock(syncLockPath(cfg), true, 2*time.Minute)
	if err != nil {
		return err
	}
	defer release()

	syncer, err := sync.NewSyncer(cfg, quiet)
	if err != nil {
		return err
	}
	if push {
		return runMCPPush(ctx, syncer)
	}
	return runMCPPull(ctx, syncer)
}

// runPushPull is the shared body of push and pull: one two-way sync, then the
// MCP push or pull when enabled, then after (pull's optional history rebuild).
func runPushPull(includeMCP, mcpPush bool, after func() error) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	ctx := context.Background()

	if !quiet {
		fmt.Printf("%s⋯ %s (same as 'stradl sync')%s\n", colorDim, twoWaySyncNote, colorReset)
	}

	start := time.Now()
	report, err := runTwoWaySync(ctx, cfg)
	if errors.Is(err, sync.ErrLocked) {
		return fmt.Errorf("another sync is still running; try again in a minute")
	}
	if err != nil {
		return err
	}
	if !quiet {
		printSyncReport(report, time.Since(start))
	}

	if includeMCP || cfg.IsMCPSyncEnabled() {
		if err := runMCPSync(ctx, cfg, mcpPush); err != nil {
			return err
		}
	}

	if after != nil {
		if err := after(); err != nil {
			return err
		}
	}

	if len(report.Errors) > 0 {
		return fmt.Errorf("%d file(s) failed to sync", len(report.Errors))
	}
	return nil
}

func pushCmd() *cobra.Command {
	var includeMCP bool

	cmd := &cobra.Command{
		Use:   "push",
		Short: "Two-way sync with your other devices (same as 'stradl sync')",
		Long: `Run a full two-way sync, exactly like 'stradl sync'.

In claude-sync, push uploaded local changes and deleted remote copies of files
deleted here. Stradl never deletes remote copies: deleting a session on one Mac
only removes it there, and sessions changed on two Macs are merged.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runPushPull(includeMCP, true, nil)
		},
	}

	cmd.Flags().BoolVar(&includeMCP, "include-mcp", false, "Also push MCP server configs from ~/.claude.json")
	return cmd
}

func pullCmd() *cobra.Command {
	var dryRun, force, includeMCP, rebuildHistory bool

	cmd := &cobra.Command{
		Use:   "pull",
		Short: "Two-way sync with your other devices (same as 'stradl sync')",
		Long: `Run a full two-way sync, exactly like 'stradl sync'.

In claude-sync, pull overwrote local files whenever the remote copy looked
newer by the clock. Stradl compares versions instead of clocks and merges
sessions changed on two Macs, so local work is never overwritten.

Examples:
  stradl pull                    # Two-way sync
  stradl pull --rebuild-history  # ...then rebuild ~/.claude/history.jsonl
  stradl diff                    # Compare with remote without changing anything`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if dryRun {
				fmt.Println("Two-way sync has no dry run. 'stradl diff' compares this device with remote without changing anything.")
				return nil
			}
			var after func() error
			if rebuildHistory {
				after = func() error {
					if err := runHistoryRebuild(); err != nil {
						return fmt.Errorf("rebuilding history: %w", err)
					}
					return nil
				}
			}
			return runPushPull(includeMCP, false, after)
		},
	}

	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Show how to preview changes ('stradl diff') without syncing")
	cmd.Flags().BoolVar(&force, "force", false, "No longer needed")
	_ = cmd.Flags().MarkDeprecated("force", "two-way sync never overwrites local files, so there is nothing to confirm")
	cmd.Flags().BoolVar(&includeMCP, "include-mcp", false, "Also pull MCP server configs into ~/.claude.json")
	cmd.Flags().BoolVar(&rebuildHistory, "rebuild-history", false, "Rebuild ~/.claude/history.jsonl from session files after syncing")

	return cmd
}

func statusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show pending local changes",
		Long:  `Display files that have been added, modified, or deleted locally.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load()
			if err != nil {
				return err
			}

			syncer, err := sync.NewSyncer(cfg, quiet)
			if err != nil {
				return err
			}

			ctx := context.Background()
			changes, err := syncer.Status(ctx)
			if err != nil {
				return err
			}

			if len(changes) == 0 {
				fmt.Println("No local changes")
				return nil
			}

			fmt.Printf("%d change(s):\n\n", len(changes))

			var added, modified, deleted []sync.FileChange
			for _, c := range changes {
				switch c.Action {
				case "add":
					added = append(added, c)
				case "modify":
					modified = append(modified, c)
				case "delete":
					deleted = append(deleted, c)
				}
			}

			if len(added) > 0 {
				fmt.Println("New files:")
				for _, c := range added {
					fmt.Printf("  + %s (%s)\n", c.Path, util.FormatSize(c.LocalSize))
				}
				fmt.Println()
			}

			if len(modified) > 0 {
				fmt.Println("Modified files:")
				for _, c := range modified {
					fmt.Printf("  ~ %s (%s)\n", c.Path, util.FormatSize(c.LocalSize))
				}
				fmt.Println()
			}

			if len(deleted) > 0 {
				fmt.Println("Deleted files:")
				for _, c := range deleted {
					fmt.Printf("  - %s\n", c.Path)
				}
				fmt.Println()
			}

			state := syncer.GetState()
			if !state.LastPush.IsZero() {
				fmt.Printf("Last push: %s\n", state.LastPush.Format(time.RFC3339))
			}
			if !state.LastPull.IsZero() {
				fmt.Printf("Last pull: %s\n", state.LastPull.Format(time.RFC3339))
			}

			return nil
		},
	}
}

func diffCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "diff",
		Short: "Show differences between local and remote",
		Long:  `Compare local ~/.claude with remote cloud storage.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load()
			if err != nil {
				return err
			}

			syncer, err := sync.NewSyncer(cfg, quiet)
			if err != nil {
				return err
			}

			ctx := context.Background()
			entries, err := syncer.Diff(ctx)
			if err != nil {
				return err
			}

			if len(entries) == 0 {
				fmt.Println("No files found")
				return nil
			}

			var localOnly, remoteOnly, modified, synced []sync.DiffEntry
			for _, e := range entries {
				switch e.Status {
				case "local_only":
					localOnly = append(localOnly, e)
				case "remote_only":
					remoteOnly = append(remoteOnly, e)
				case "modified":
					modified = append(modified, e)
				case "synced":
					synced = append(synced, e)
				}
			}

			if len(localOnly) > 0 {
				fmt.Printf("Local only (%d files):\n", len(localOnly))
				for _, e := range localOnly {
					fmt.Printf("  + %s (%s)\n", e.Path, util.FormatSize(e.LocalSize))
				}
				fmt.Println()
			}

			if len(remoteOnly) > 0 {
				fmt.Printf("Remote only (%d files):\n", len(remoteOnly))
				for _, e := range remoteOnly {
					fmt.Printf("  - %s (%s)\n", e.Path, util.FormatSize(e.RemoteSize))
				}
				fmt.Println()
			}

			if len(modified) > 0 {
				fmt.Printf("Modified (%d files):\n", len(modified))
				for _, e := range modified {
					fmt.Printf("  ~ %s (local: %s, remote: %s)\n", e.Path, util.FormatSize(e.LocalSize), util.FormatSize(e.RemoteSize))
				}
				fmt.Println()
			}

			fmt.Printf("Summary: %d synced, %d local only, %d remote only, %d modified\n",
				len(synced), len(localOnly), len(remoteOnly), len(modified))

			return nil
		},
	}
}

type conflictFile struct {
	ConflictPath string
	OriginalPath string
	Timestamp    string
}

func conflictsCmd() *cobra.Command {
	var listOnly bool
	var resolveAll string

	cmd := &cobra.Command{
		Use:   "conflicts",
		Short: "List and resolve sync conflicts",
		Long: `Find and resolve conflicts from sync operations.

When both local and remote files change, the remote version is saved
as a .conflict file. Use this command to review and resolve them.

Examples:
  stradl conflicts              # Interactive resolution
  stradl conflicts --list       # Just list conflicts
  stradl conflicts --keep local # Keep all local versions
  stradl conflicts --keep remote # Keep all remote versions`,
		RunE: func(cmd *cobra.Command, args []string) error {
			claudeDir := config.ClaudeDir()

			// Find all .conflict files
			conflicts, err := findConflicts(claudeDir)
			if err != nil {
				return err
			}

			if len(conflicts) == 0 {
				fmt.Printf("%s✓%s No conflicts found\n", colorGreen, colorReset)
				return nil
			}

			fmt.Printf("%sFound %d conflict(s):%s\n\n", colorYellow, len(conflicts), colorReset)

			for i, c := range conflicts {
				relOriginal, _ := filepath.Rel(claudeDir, c.OriginalPath)
				fmt.Printf("  %s%d.%s %s\n", colorCyan, i+1, colorReset, relOriginal)
				fmt.Printf("     %sConflict from: %s%s\n", colorDim, c.Timestamp, colorReset)
			}
			fmt.Println()

			// List only mode
			if listOnly {
				return nil
			}

			// Load sync state to update after resolution
			state, err := sync.LoadState()
			if err != nil {
				return fmt.Errorf("failed to load sync state: %w", err)
			}

			// Batch resolve mode
			if resolveAll != "" {
				return batchResolveConflicts(conflicts, resolveAll, claudeDir, state)
			}

			// Interactive mode
			return interactiveResolveConflicts(conflicts, claudeDir, state)
		},
	}

	cmd.Flags().BoolVarP(&listOnly, "list", "l", false, "Only list conflicts, don't resolve")
	cmd.Flags().StringVar(&resolveAll, "keep", "", "Resolve all conflicts: 'local' or 'remote'")

	return cmd
}

func findConflicts(claudeDir string) ([]conflictFile, error) {
	var conflicts []conflictFile

	err := filepath.Walk(claudeDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil // Skip errors
		}
		if info.IsDir() {
			return nil
		}

		// Look for .conflict. pattern
		if strings.Contains(filepath.Base(path), ".conflict.") {
			// Extract original path and timestamp
			// Format: filename.ext.conflict.20260208-095132
			parts := strings.Split(path, ".conflict.")
			if len(parts) == 2 {
				conflicts = append(conflicts, conflictFile{
					ConflictPath: path,
					OriginalPath: parts[0],
					Timestamp:    parts[1],
				})
			}
		}
		return nil
	})

	// Sort by timestamp (newest first)
	sort.Slice(conflicts, func(i, j int) bool {
		return conflicts[i].Timestamp > conflicts[j].Timestamp
	})

	return conflicts, err
}

func batchResolveConflicts(conflicts []conflictFile, keep string, claudeDir string, state *sync.SyncState) error {
	keep = strings.ToLower(keep)
	if keep != "local" && keep != "remote" {
		return fmt.Errorf("--keep must be 'local' or 'remote'")
	}

	resolved := 0
	for _, c := range conflicts {
		if keep == "local" {
			// Delete conflict file, keep local
			if err := os.Remove(c.ConflictPath); err != nil {
				fmt.Printf("%s✗%s Failed to remove %s: %v\n", colorYellow, colorReset, c.ConflictPath, err)
				continue
			}
			fmt.Printf("%s✓%s Kept local: %s\n", colorGreen, colorReset, filepath.Base(c.OriginalPath))
		} else {
			// Replace local with conflict, delete conflict
			if err := os.Rename(c.ConflictPath, c.OriginalPath); err != nil {
				fmt.Printf("%s✗%s Failed to replace %s: %v\n", colorYellow, colorReset, c.OriginalPath, err)
				continue
			}
			fmt.Printf("%s✓%s Kept remote: %s\n", colorGreen, colorReset, filepath.Base(c.OriginalPath))
		}

		// Update state with the resolved file's hash
		relPath, _ := filepath.Rel(claudeDir, c.OriginalPath)
		if info, err := os.Stat(c.OriginalPath); err == nil {
			if hash, err := sync.HashFile(c.OriginalPath); err == nil {
				state.UpdateFile(relPath, info, hash)
				state.MarkUploaded(relPath)
			}
		}
		resolved++
	}

	// Save updated state
	if resolved > 0 {
		if err := state.Save(); err != nil {
			fmt.Printf("%s⚠%s Warning: failed to save state: %v\n", colorYellow, colorReset, err)
		}
	}

	fmt.Printf("\n%s✓%s Resolved %d conflict(s)\n", colorGreen, colorReset, resolved)
	return nil
}

func interactiveResolveConflicts(conflicts []conflictFile, claudeDir string, state *sync.SyncState) error {
	reader := bufio.NewReader(os.Stdin)

	fmt.Println("For each conflict, choose how to resolve:")
	fmt.Printf("  %s[l]%s Keep local  %s[r]%s Keep remote  %s[d]%s Show diff  %s[s]%s Skip  %s[q]%s Quit\n\n",
		colorCyan, colorReset,
		colorCyan, colorReset,
		colorCyan, colorReset,
		colorCyan, colorReset,
		colorCyan, colorReset)

	resolved := 0
	for i, c := range conflicts {
		relOriginal, _ := filepath.Rel(claudeDir, c.OriginalPath)

		// Get file sizes for context
		localInfo, _ := os.Stat(c.OriginalPath)
		conflictInfo, _ := os.Stat(c.ConflictPath)

		localSize := int64(0)
		conflictSize := int64(0)
		if localInfo != nil {
			localSize = localInfo.Size()
		}
		if conflictInfo != nil {
			conflictSize = conflictInfo.Size()
		}

		fmt.Printf("%s[%d/%d]%s %s\n", colorCyan, i+1, len(conflicts), colorReset, relOriginal)
		fmt.Printf("        Local: %s  |  Remote: %s  |  Conflict from: %s\n",
			util.FormatSize(localSize), util.FormatSize(conflictSize), c.Timestamp)

	promptLoop:
		for {
			fmt.Printf("        %sResolve [l/r/d/s/q]:%s ", colorDim, colorReset)
			input, _ := reader.ReadString('\n')
			input = strings.TrimSpace(strings.ToLower(input))

			switch input {
			case "l", "local":
				// Keep local, delete conflict
				if err := os.Remove(c.ConflictPath); err != nil {
					fmt.Printf("        %s✗%s Error: %v\n", colorYellow, colorReset, err)
				} else {
					fmt.Printf("        %s✓%s Kept local version\n\n", colorGreen, colorReset)
					// Update state with the kept file's hash
					if info, err := os.Stat(c.OriginalPath); err == nil {
						if hash, err := sync.HashFile(c.OriginalPath); err == nil {
							state.UpdateFile(relOriginal, info, hash)
							state.MarkUploaded(relOriginal)
						}
					}
					resolved++
				}
				break promptLoop

			case "r", "remote":
				// Replace local with conflict
				if err := os.Rename(c.ConflictPath, c.OriginalPath); err != nil {
					fmt.Printf("        %s✗%s Error: %v\n", colorYellow, colorReset, err)
				} else {
					fmt.Printf("        %s✓%s Replaced with remote version\n\n", colorGreen, colorReset)
					// Update state with the new file's hash
					if info, err := os.Stat(c.OriginalPath); err == nil {
						if hash, err := sync.HashFile(c.OriginalPath); err == nil {
							state.UpdateFile(relOriginal, info, hash)
							state.MarkUploaded(relOriginal)
						}
					}
					resolved++
				}
				break promptLoop

			case "d", "diff":
				// Show diff
				showDiff(c.OriginalPath, c.ConflictPath)

			case "s", "skip":
				fmt.Printf("        %s→%s Skipped\n\n", colorDim, colorReset)
				break promptLoop

			case "q", "quit":
				// Save state before quitting if any conflicts were resolved
				if resolved > 0 {
					if err := state.Save(); err != nil {
						fmt.Printf("%s⚠%s Warning: failed to save state: %v\n", colorYellow, colorReset, err)
					}
				}
				fmt.Printf("\n%s✓%s Resolved %d of %d conflict(s)\n", colorGreen, colorReset, resolved, len(conflicts))
				return nil

			default:
				fmt.Printf("        %sInvalid choice. Use l/r/d/s/q%s\n", colorDim, colorReset)
			}
		}
	}

	// Save updated state
	if resolved > 0 {
		if err := state.Save(); err != nil {
			fmt.Printf("%s⚠%s Warning: failed to save state: %v\n", colorYellow, colorReset, err)
		}
	}

	fmt.Printf("%s✓%s Resolved %d of %d conflict(s)\n", colorGreen, colorReset, resolved, len(conflicts))
	return nil
}

func showDiff(localPath, conflictPath string) {
	// Try to use diff command
	cmd := exec.Command("diff", "-u", "--color=always", localPath, conflictPath)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	fmt.Println()
	fmt.Printf("        %s--- Local%s\n", colorGreen, colorReset)
	fmt.Printf("        %s+++ Remote (conflict)%s\n", colorCyan, colorReset)
	fmt.Println()

	if err := cmd.Run(); err != nil {
		// diff returns exit code 1 when files differ, which is expected
		if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 1 {
			// Files differ, this is normal
		} else if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 2 {
			// diff command failed
			fmt.Printf("        %sCould not run diff command%s\n", colorDim, colorReset)

			// Fall back to showing file sizes
			localInfo, _ := os.Stat(localPath)
			conflictInfo, _ := os.Stat(conflictPath)
			if localInfo != nil && conflictInfo != nil {
				fmt.Printf("        Local:  %s (%s)\n", localPath, util.FormatSize(localInfo.Size()))
				fmt.Printf("        Remote: %s (%s)\n", conflictPath, util.FormatSize(conflictInfo.Size()))
			}
		}
	}
	fmt.Println()
}

func resetCmd() *cobra.Command {
	var clearRemote, clearLocal, force bool

	cmd := &cobra.Command{
		Use:   "reset",
		Short: "Reset stradl (clear data and start fresh)",
		Long: `Reset stradl configuration and optionally clear remote/local data.

Use this if you forgot your passphrase or want to start fresh.

Examples:
  stradl reset                    # Clear local config only
  stradl reset --remote           # Also delete all files from cloud storage
  stradl reset --local            # Also clear local sync state
  stradl reset --remote --local   # Full reset (nuclear option)`,
		RunE: func(cmd *cobra.Command, args []string) error {
			reader := bufio.NewReader(os.Stdin)

			fmt.Println()
			printWarning("This will reset stradl:")
			fmt.Println()

			if clearRemote {
				fmt.Printf("  %s•%s Delete ALL files from cloud storage bucket\n", colorYellow, colorReset)
			}
			if clearLocal {
				fmt.Printf("  %s•%s Clear local sync state\n", colorYellow, colorReset)
			}
			fmt.Printf("  %s•%s Delete local config and encryption key\n", colorYellow, colorReset)
			fmt.Println()

			if !force {
				fmt.Printf("%sType 'reset' to confirm:%s ", colorYellow, colorReset)
				confirm, _ := reader.ReadString('\n')
				if strings.TrimSpace(confirm) != "reset" {
					fmt.Println("Aborted.")
					return nil
				}
				fmt.Println()
			}

			// Clear remote if requested
			if clearRemote {
				fmt.Printf("%s⋯%s Deleting remote files...\n", colorDim, colorReset)

				cfg, err := config.Load()
				if err != nil {
					printWarning("Could not load config: " + err.Error())
				} else {
					storageCfg := cfg.GetStorageConfig()
					store, err := storage.New(storageCfg)
					if err != nil {
						printWarning("Could not connect to storage: " + err.Error())
					} else {
						ctx := context.Background()
						objects, err := store.List(ctx, "")
						if err != nil {
							printWarning("Could not list objects: " + err.Error())
						} else {
							deleted := 0
							for _, obj := range objects {
								if err := store.Delete(ctx, obj.Key); err != nil {
									printWarning("Failed to delete " + obj.Key)
								} else {
									deleted++
								}
							}
							printSuccess(fmt.Sprintf("Deleted %d files from storage", deleted))
						}
					}
				}
			}

			// Clear local state if requested
			if clearLocal {
				statePath := config.StateFilePath()
				if err := os.Remove(statePath); err != nil && !os.IsNotExist(err) {
					printWarning("Could not remove state file: " + err.Error())
				} else {
					printSuccess("Cleared local sync state")
				}
			}

			// Always clear config and key
			configDir := config.ConfigDirPath()
			if err := os.RemoveAll(configDir); err != nil {
				return fmt.Errorf("failed to remove config directory: %w", err)
			}
			printSuccess("Removed " + configDir)

			fmt.Println()
			printSuccess("Reset complete!")
			fmt.Println()
			printInfo("Run 'stradl init' to set up again with a new passphrase.")
			fmt.Println()

			return nil
		},
	}

	cmd.Flags().BoolVar(&clearRemote, "remote", false, "Delete all files from cloud storage bucket")
	cmd.Flags().BoolVar(&clearLocal, "local", false, "Clear local sync state")
	cmd.Flags().BoolVar(&force, "force", false, "Skip confirmation prompt")

	return cmd
}

func rebuildHistoryCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "rebuild-history",
		Short: "Rebuild ~/.claude/history.jsonl from session files",
		Long: `Reconstruct the prompt history by merging the current history.jsonl with
user prompts extracted from session files under ~/.claude/projects/.

history.jsonl is synced as a single file, so pushes from two devices are
last-writer-wins and one device's entries can be lost, breaking the /resume
session picker. Session files sync cleanly (one file per session), so the
full history can always be rebuilt from them.

Existing entries are preserved as-is; recovered entries are merged in,
deduplicated, and sorted by timestamp. The previous file is kept as
history.jsonl.bak.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runHistoryRebuild()
		},
	}
}

// runHistoryRebuild rebuilds history.jsonl and reports how many prompts were
// recovered from session files.
func runHistoryRebuild() error {
	claudeDir, err := config.ClaudeDirE()
	if err != nil {
		return err
	}
	result, err := sync.RebuildHistory(claudeDir)
	if err != nil {
		return err
	}
	recovered := result.Merged - result.Existing
	printSuccess(fmt.Sprintf("Rebuilt history.jsonl: %d entries (%d existing + %d recovered from sessions)",
		result.Merged, result.Existing, recovered))
	return nil
}

func migrateCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "migrate",
		Short: "Rewrite legacy remote keys to portable path-mapped keys",
		Long: `Re-upload this device's project files under portable, path-mapped remote
keys and delete the legacy machine-specific keys.

Older versions stored project sessions under keys derived from this machine's
absolute paths (e.g. projects/-Users-alice-my-app/...), so sessions pulled on
a device with a different username or layout were not resumable. New pushes
use portable tokens (e.g. projects/${HOME}-my-app/...); migrate converts
existing remote data in place.

Run this once on every device. Keys owned by another device are left for that
device's migrate run and reported as "left for other devices".`,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load()
			if err != nil {
				return err
			}

			syncer, err := sync.NewSyncer(cfg, quiet)
			if err != nil {
				return err
			}

			if !quiet {
				syncer.SetProgressFunc(func(event sync.ProgressEvent) {
					if event.Error != nil {
						fmt.Printf("\r%s✗%s %s: %v\n", colorYellow, colorReset, event.Path, event.Error)
						return
					}
					if event.Action == "upload" && !event.Complete {
						progress := fmt.Sprintf("[%d/%d]", event.Current, event.Total)
						shortPath := util.TruncatePath(event.Path, 50)
						fmt.Printf("\r%s↻%s %s%s%s %s%s",
							colorCyan, colorReset,
							colorDim, progress, colorReset,
							shortPath,
							strings.Repeat(" ", 10))
					}
				})
				fmt.Printf("%s⋯%s Scanning remote for legacy keys...\n", colorDim, colorReset)
			}

			result, err := syncer.MigratePaths(context.Background())
			if err != nil {
				return err
			}

			if !quiet {
				fmt.Println()
				if len(result.Migrated) == 0 && len(result.Foreign) == 0 && len(result.Errors) == 0 {
					fmt.Printf("%s✓%s Nothing to migrate\n", colorGreen, colorReset)
					return nil
				}

				var parts []string
				if len(result.Migrated) > 0 {
					parts = append(parts, fmt.Sprintf("%s%d migrated%s", colorGreen, len(result.Migrated), colorReset))
				}
				if len(result.Foreign) > 0 {
					parts = append(parts, fmt.Sprintf("%s%d left for other devices%s", colorDim, len(result.Foreign), colorReset))
				}
				if len(result.Errors) > 0 {
					parts = append(parts, fmt.Sprintf("%s%d failed%s", colorYellow, len(result.Errors), colorReset))
				}
				fmt.Printf("%s✓%s Migration complete: %s\n", colorGreen, colorReset, strings.Join(parts, ", "))

				if len(result.Foreign) > 0 {
					fmt.Printf("\n%sRun 'stradl migrate' on your other devices to convert the remaining keys.%s\n", colorDim, colorReset)
				}
				if len(result.Errors) > 0 {
					fmt.Printf("\n%sErrors:%s\n", colorYellow, colorReset)
					for _, e := range result.Errors {
						fmt.Printf("  %s•%s %v\n", colorYellow, colorReset, e)
					}
				}
			}

			return nil
		},
	}

	return cmd
}

// verifyKeyMatchesRemote checks if the encryption key can decrypt existing remote files.
// Returns nil if no files exist or if decryption succeeds.
// Returns an error if files exist but cannot be decrypted with the current key.
func verifyKeyMatchesRemote(ctx context.Context, store storage.Storage, keyPath string) error {
	// List remote files
	objects, err := store.List(ctx, "")
	if err != nil || len(objects) == 0 {
		return nil // No files to verify, or error listing (will fail later anyway)
	}

	// Find a small file to test with (prefer smaller files for faster verification)
	var testObj storage.ObjectInfo
	for _, obj := range objects {
		if obj.Size > 0 && obj.Size < 10000 { // Pick a small file under 10KB
			testObj = obj
			break
		}
	}
	if testObj.Key == "" && len(objects) > 0 {
		testObj = objects[0] // Fallback to first file
	}
	if testObj.Key == "" {
		return nil // No suitable file found
	}

	// Download the test file
	encrypted, err := store.Download(ctx, testObj.Key)
	if err != nil {
		return nil // Download failed, will fail later during pull
	}

	// Try to decrypt with current key
	enc, err := crypto.NewEncryptor(keyPath)
	if err != nil {
		return fmt.Errorf("failed to load encryption key: %w", err)
	}

	_, err = enc.Decrypt(encrypted)
	if err != nil {
		return fmt.Errorf("key_mismatch: cannot decrypt remote files with current key")
	}

	return nil
}

// keyMismatchAction represents the user's choice when a key mismatch is detected
type keyMismatchAction int

const (
	actionRetryPassphrase keyMismatchAction = iota
	actionClearRemote
	actionAbort
)

// handleKeyMismatch displays a helpful error message and prompts the user for action
func handleKeyMismatch() (keyMismatchAction, error) {
	fmt.Println()
	printWarning("Cannot decrypt existing remote files!")
	fmt.Println()
	printInfo("The bucket contains files encrypted with a different key.")
	printInfo("This happens when:")
	printInfo("  - You used a different passphrase on another device")
	printInfo("  - You previously set up with a random key (not passphrase)")
	fmt.Println()

	prompt := &survey.Select{
		Message: "What would you like to do?",
		Options: []string{
			"Try a different passphrase",
			"Clear remote files and start fresh (your local ~/.claude will be pushed)",
			"Abort setup",
		},
	}
	var choice int
	if err := survey.AskOne(prompt, &choice); err != nil {
		return actionAbort, err
	}

	switch choice {
	case 0:
		return actionRetryPassphrase, nil
	case 1:
		return actionClearRemote, nil
	default:
		return actionAbort, nil
	}
}

// clearRemoteStorage deletes all files from the remote storage bucket
func clearRemoteStorage(ctx context.Context, store storage.Storage) error {
	objects, err := store.List(ctx, "")
	if err != nil {
		return fmt.Errorf("failed to list remote files: %w", err)
	}

	if len(objects) == 0 {
		return nil
	}

	keys := make([]string, len(objects))
	for i, obj := range objects {
		keys[i] = obj.Key
	}

	return store.DeleteBatch(ctx, keys)
}

// MCP sync commands

func mcpCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "mcp",
		Short: "Manage MCP server sync",
		Long:  `Sync global MCP server configurations from ~/.claude.json across devices.`,
	}
	cmd.AddCommand(
		mcpStatusCmd(),
		mcpEnableCmd(),
		mcpDisableCmd(),
		mcpListCmd(),
		mcpPushCmd(),
		mcpPullCmd(),
	)
	return cmd
}

func mcpStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show MCP sync settings and local server state",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load()
			if err != nil {
				return err
			}

			// Auto-sync setting
			if cfg.IsMCPSyncEnabled() {
				fmt.Printf("  Auto-sync  %s✓ enabled%s  (included in every push/pull)\n", colorGreen, colorReset)
			} else {
				fmt.Printf("  Auto-sync  %s✗ disabled%s  (use --include-mcp or 'mcp push/pull')\n", colorDim, colorReset)
			}

			// Local server count + pending changes
			syncer, err := sync.NewSyncer(cfg, quiet)
			if err != nil {
				return err
			}
			ctx := context.Background()
			status, err := syncer.MCPStatus(ctx)
			if err != nil {
				return err
			}

			if status.ServerCount == 0 {
				fmt.Printf("  Servers    %s0 servers%s in %s\n", colorDim, colorReset, config.ClaudeJSONPath())
			} else {
				fmt.Printf("  Servers    %d configured\n", status.ServerCount)
			}

			if status.HasChanges {
				fmt.Printf("  Changes    %s● unpushed local changes%s\n", colorYellow, colorReset)
			} else {
				fmt.Printf("  Changes    %s✓ in sync%s\n", colorGreen, colorReset)
			}

			fmt.Printf("\n  %smcp enable%s   — auto-include in every push/pull\n", colorDim, colorReset)
			fmt.Printf("  %smcp disable%s  — manual only\n", colorDim, colorReset)

			return nil
		},
	}
}

func mcpEnableCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "enable",
		Short: "Enable automatic MCP sync on every push/pull (syncs now too)",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load()
			if err != nil {
				return err
			}

			cfg.SetMCPSync(true)
			if err := config.Save(cfg); err != nil {
				return err
			}
			fmt.Printf("%s✓%s MCP auto-sync enabled.\n", colorGreen, colorReset)

			// Push current state immediately
			return runMCPLocked(context.Background(), cfg, true)
		},
	}
}

func mcpDisableCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "disable",
		Short: "Disable automatic MCP sync on every push/pull",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load()
			if err != nil {
				return err
			}

			cfg.SetMCPSync(false)
			if err := config.Save(cfg); err != nil {
				return err
			}
			fmt.Printf("%s✓%s MCP sync disabled. Use --include-mcp flag for one-time sync.\n", colorGreen, colorReset)
			return nil
		},
	}
}

func mcpListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List local MCP server configurations",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load()
			if err != nil {
				return err
			}

			syncer, err := sync.NewSyncer(cfg, quiet)
			if err != nil {
				return err
			}

			ctx := context.Background()
			status, err := syncer.MCPStatus(ctx)
			if err != nil {
				return err
			}

			if status.ServerCount == 0 {
				fmt.Printf("%s⋯%s No MCP servers found in %s\n", colorDim, colorReset, config.ClaudeJSONPath())
				return nil
			}

			fmt.Printf("%sMCP Servers%s (%d total):\n\n", colorBold, colorReset, status.ServerCount)
			for name := range status.Servers {
				syncMark := fmt.Sprintf("%s●%s", colorGreen, colorReset)
				if status.HasChanges {
					syncMark = fmt.Sprintf("%s○%s", colorYellow, colorReset)
				}
				fmt.Printf("  %s %s\n", syncMark, name)
			}

			if status.HasChanges {
				fmt.Printf("\n%s○ = has local changes not yet pushed%s\n", colorDim, colorReset)
			}

			return nil
		},
	}
}

func mcpPushCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "push",
		Short: "Push MCP server configs to cloud storage",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load()
			if err != nil {
				return err
			}
			return runMCPLocked(context.Background(), cfg, true)
		},
	}
}

func mcpPullCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "pull",
		Short: "Pull MCP server configs from cloud storage",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load()
			if err != nil {
				return err
			}
			return runMCPLocked(context.Background(), cfg, false)
		},
	}
}

func runMCPPush(ctx context.Context, syncer *sync.Syncer) error {
	result, err := syncer.PushMCP(ctx)
	if err != nil {
		return fmt.Errorf("MCP push failed: %w", err)
	}

	if !quiet {
		if result.Unchanged {
			fmt.Printf("%s✓%s MCP servers: no changes to push\n", colorGreen, colorReset)
		} else {
			fmt.Printf("%s✓%s MCP servers: %s%d servers pushed%s\n",
				colorGreen, colorReset, colorGreen, result.ServersPushed, colorReset)
		}
	}
	return nil
}

func runMCPPull(ctx context.Context, syncer *sync.Syncer) error {
	result, err := syncer.PullMCP(ctx)
	if err != nil {
		return fmt.Errorf("MCP pull failed: %w", err)
	}

	if !quiet {
		if result.NoRemote {
			fmt.Printf("%s⋯%s MCP servers: no remote data found\n", colorDim, colorReset)
			return nil
		}

		var parts []string
		if len(result.Added) > 0 {
			parts = append(parts, fmt.Sprintf("%s%d added%s", colorGreen, len(result.Added), colorReset))
			for _, name := range result.Added {
				fmt.Printf("  %s+%s %s\n", colorGreen, colorReset, name)
			}
		}
		if len(result.Updated) > 0 {
			parts = append(parts, fmt.Sprintf("%s%d updated%s", colorCyan, len(result.Updated), colorReset))
			for _, name := range result.Updated {
				fmt.Printf("  %s~%s %s\n", colorCyan, colorReset, name)
			}
		}
		if len(result.Kept) > 0 {
			parts = append(parts, fmt.Sprintf("%d unchanged", len(result.Kept)))
		}
		if len(result.Conflicts) > 0 {
			parts = append(parts, fmt.Sprintf("%s%d conflicts%s", colorYellow, len(result.Conflicts), colorReset))
			for _, c := range result.Conflicts {
				fmt.Printf("  %s!%s %s (kept local version)\n", colorYellow, colorReset, c.Key)
			}
		}

		if len(parts) > 0 {
			fmt.Printf("%s✓%s MCP pull complete: %s\n", colorGreen, colorReset, strings.Join(parts, ", "))
		} else {
			fmt.Printf("%s✓%s MCP servers: already up to date\n", colorGreen, colorReset)
		}
	}
	return nil
}

// autoCmd handles the Claude Code hooks older claude-sync versions installed.
// Stradl syncs from a background agent instead, so it only reports and
// removes those hooks; it never installs new ones.
func autoCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "auto",
		Short: "Legacy Claude Code hooks (Stradl uses a background agent instead)",
		Long: `claude-sync could sync from Claude Code hooks: pull on SessionStart and push
on Stop, stored in ~/.claude/settings.json. Stradl keeps every Mac in sync
with a background agent instead; run 'stradl agent install'.

'auto disable' removes leftover claude-sync hooks, and 'auto status' reports
whether any are still installed.`,
	}

	cmd.AddCommand(
		autoEnableCmd(),
		autoDisableCmd(),
		autoStatusCmd(),
	)

	return cmd
}

// legacyHookStatus reports which claude-sync hooks are installed in Claude
// Code's settings.json.
func legacyHookStatus() (claudesettings.AutoSyncStatus, error) {
	settings, err := claudesettings.Load(claudesettings.SettingsPath(""))
	if err != nil {
		return claudesettings.AutoSyncStatus{}, fmt.Errorf("failed to load settings: %w", err)
	}
	return settings.AutoSyncStatus(), nil
}

func printLegacyHooks(status claudesettings.AutoSyncStatus) {
	if status.HasSessionStart {
		fmt.Printf("    SessionStart → %s\n", claudesettings.HookCommandPull)
	}
	if status.HasStop {
		fmt.Printf("    Stop → %s\n", claudesettings.HookCommandPush)
	}
}

func autoEnableCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "enable",
		Short: "Explain how to turn on background sync (no hooks are installed)",
		Long: `Stradl does not install Claude Code hooks. Background sync is handled by the
Stradl agent, which you install with 'stradl agent install'.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			fmt.Printf("%s⋯%s Stradl syncs with a background agent, not Claude Code hooks.\n", colorDim, colorReset)
			fmt.Printf("    Run '%sstradl agent install%s' to keep this Mac in sync automatically.\n", colorCyan, colorReset)

			status, err := legacyHookStatus()
			if err != nil {
				return err
			}
			if status.Enabled {
				fmt.Println()
				printWarning("Legacy claude-sync hooks are still installed:")
				printLegacyHooks(status)
				fmt.Printf("    Remove them with '%sstradl auto disable%s'.\n", colorCyan, colorReset)
			}
			return nil
		},
	}
}

func autoDisableCmd() *cobra.Command {
	var dryRun bool

	cmd := &cobra.Command{
		Use:   "disable",
		Short: "Remove legacy claude-sync hooks from Claude Code",
		Long: `Removes claude-sync hooks from ~/.claude/settings.json.
Other hooks are preserved. This command is idempotent.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			path := claudesettings.SettingsPath("")

			settings, err := claudesettings.Load(path)
			if err != nil {
				return fmt.Errorf("failed to load settings: %w", err)
			}

			changed := settings.DisableAutoSync()

			if !changed {
				if !quiet {
					fmt.Printf("%s✓%s No legacy claude-sync hooks installed\n", colorGreen, colorReset)
				}
				return nil
			}

			if dryRun {
				fmt.Printf("%s⋯%s Dry run: would remove legacy claude-sync hooks\n", colorDim, colorReset)
				return nil
			}

			if err := settings.Save(path); err != nil {
				return fmt.Errorf("failed to save settings: %w", err)
			}

			if !quiet {
				fmt.Printf("%s✓%s Legacy claude-sync hooks removed\n", colorGreen, colorReset)
			}

			return nil
		},
	}

	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Show what would be changed without modifying files")

	return cmd
}

func autoStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Report legacy claude-sync hooks, if any",
		RunE: func(cmd *cobra.Command, args []string) error {
			status, err := legacyHookStatus()
			if err != nil {
				return err
			}

			if status.Enabled {
				fmt.Printf("%s!%s Legacy claude-sync hooks are installed:\n", colorYellow, colorReset)
				printLegacyHooks(status)
				fmt.Printf("    They run the old claude-sync, not Stradl. Remove them with '%sstradl auto disable%s'.\n", colorCyan, colorReset)
			} else {
				fmt.Printf("%s✓%s No legacy claude-sync hooks installed\n", colorGreen, colorReset)
			}
			fmt.Printf("    Background sync is handled by the Stradl agent ('%sstradl agent install%s').\n", colorCyan, colorReset)

			return nil
		},
	}
}

// pathsCmd manages sync paths and exclude filters
func pathsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "paths",
		Short: "Manage sync paths and exclude filters",
		Long: `Control which paths under ~/.claude/ are synced.

Effective sync = sync_list − exclude_list

Use 'paths add' to include a path, 'paths remove' to exclude it.
Use 'paths exclude' for sub-path glob filters (e.g., skip node_modules inside plugins/).`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runPathsList()
		},
	}
	cmd.AddCommand(
		pathsListCmd(),
		pathsAddCmd(),
		pathsRemoveCmd(),
		pathsExcludeCmd(),
		pathsUnexcludeCmd(),
		pathsResetCmd(),
	)
	return cmd
}

func pathsListCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "list",
		Short:   "Show sync paths and exclude filters",
		Aliases: []string{"ls"},
		RunE: func(cmd *cobra.Command, args []string) error {
			return runPathsList()
		},
	}
}

func runPathsList() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	mgr := paths.NewManager(cfg.SyncPaths, cfg.Exclude, config.ClaudeDir(), cfg.Scope)
	status := mgr.Status()

	source := "default"
	if status.IsCustomized {
		source = "config.yaml"
	}

	scopeLabel := cfg.Scope
	if scopeLabel == "" {
		scopeLabel = config.ScopeFull
	}

	// List what actually syncs, not the raw config list. Under sessions scope
	// the effective set is the intersection with SessionSyncPaths, so showing
	// cfg.SyncPaths verbatim would advertise paths that are never uploaded.
	effective := make(map[string]struct{})
	for _, p := range cfg.GetEffectiveSyncPaths() {
		effective[p] = struct{}{}
	}

	var outOfScope []string

	fmt.Printf("\n%sSync Paths%s (%s, scope: %s):\n", colorBold, colorReset, source, scopeLabel)
	for _, p := range status.SyncPaths {
		if _, ok := effective[p]; !ok {
			outOfScope = append(outOfScope, p)
			continue
		}
		marker := colorGreen + "+" + colorReset
		if !mgr.IsDefault(p) {
			marker = colorCyan + "+" + colorReset + " (custom)"
		}
		fmt.Printf("  %s %s\n", marker, p)
	}

	if len(outOfScope) > 0 {
		fmt.Printf("\n%sNot synced%s (outside the %q scope):\n", colorBold, colorReset, scopeLabel)
		for _, p := range outOfScope {
			fmt.Printf("  %s-%s %s\n", colorYellow, colorReset, p)
		}
		fmt.Printf("\n  These remain in config.yaml and apply again if scope is set to %q.\n",
			config.ScopeFull)
	}

	if len(status.Excludes) > 0 {
		fmt.Printf("\n%sExclude Filters%s:\n", colorBold, colorReset)
		for _, e := range status.Excludes {
			fmt.Printf("  %s-%s %s\n", colorYellow, colorReset, e)
		}
	}

	fmt.Println()
	return nil
}

func pathsAddCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "add <path>",
		Short: "Add a path to sync",
		Long: `Add a relative path under ~/.claude/ to the sync list.

If the path was previously removed (and has an exclude), the
conflicting exclude is automatically removed.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load()
			if err != nil {
				return err
			}

			mgr := paths.NewManager(cfg.SyncPaths, cfg.Exclude, config.ClaudeDir(), cfg.Scope)
			result := mgr.Add(args[0])

			if result.Invalid != nil {
				return fmt.Errorf("invalid sync path: %w", result.Invalid)
			}

			if result.OutOfScope {
				fmt.Printf("%s!%s %s is outside the %q scope and would not be synced\n",
					colorYellow, colorReset, args[0], config.ScopeSessions)
				fmt.Printf("  Set %sscope: full%s in %s to sync paths beyond session data.\n",
					colorCyan, colorReset, config.ConfigFilePath())
				return nil
			}

			if result.AlreadyExists {
				fmt.Printf("%s!%s %s is already in the sync list\n", colorYellow, colorReset, args[0])
				return nil
			}

			if result.PathMissing {
				fmt.Printf("%s!%s %s does not exist under ~/.claude (adding anyway)\n", colorYellow, colorReset, args[0])
			}

			cfg.SyncPaths = mgr.SyncPaths()
			cfg.Exclude = mgr.Excludes()
			if err := config.Save(cfg); err != nil {
				return err
			}

			fmt.Printf("%s✓%s Added %s to sync\n", colorGreen, colorReset, args[0])
			if result.ExcludesRemoved > 0 {
				fmt.Printf("%s✓%s Removed %d conflicting exclude(s)\n", colorGreen, colorReset, result.ExcludesRemoved)
			}
			return nil
		},
	}
}

func pathsRemoveCmd() *cobra.Command {
	var force bool
	cmd := &cobra.Command{
		Use:   "remove <path>",
		Short: "Remove a path from sync",
		Long: `Remove a path from the sync list.

Default paths are also added to excludes so they stay off after reset.
Custom paths are simply removed from the list.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load()
			if err != nil {
				return err
			}

			mgr := paths.NewManager(cfg.SyncPaths, cfg.Exclude, config.ClaudeDir(), cfg.Scope)

			if !mgr.HasPath(args[0]) {
				fmt.Printf("%s!%s %s is not in the sync list\n", colorYellow, colorReset, args[0])
				return nil
			}

			if !force {
				var confirm bool
				prompt := &survey.Confirm{
					Message: fmt.Sprintf("Remove %q from sync?", args[0]),
					Default: false,
				}
				if err := survey.AskOne(prompt, &confirm); err != nil || !confirm {
					fmt.Println("  Cancelled.")
					return nil
				}
			}

			result := mgr.Remove(args[0])
			cfg.SyncPaths = mgr.SyncPaths()
			cfg.Exclude = mgr.Excludes()
			if err := config.Save(cfg); err != nil {
				return err
			}

			fmt.Printf("%s✓%s Removed %s from sync\n", colorGreen, colorReset, args[0])
			if result.ExcludeAdded != "" {
				fmt.Printf("%s✓%s Added exclude %s (survives reset)\n", colorGreen, colorReset, result.ExcludeAdded)
			}
			return nil
		},
	}
	cmd.Flags().BoolVarP(&force, "force", "f", false, "Skip confirmation")
	return cmd
}

func pathsExcludeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "exclude <glob>",
		Short: "Add a sub-path glob filter",
		Long: `Add a glob pattern to skip files inside a synced directory.

Use for filtering within a sync path (e.g., skip node_modules).
To stop syncing a whole path, use 'paths remove' instead.

Glob syntax: dir/*, dir/**, **/*.ext`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load()
			if err != nil {
				return err
			}

			mgr := paths.NewManager(cfg.SyncPaths, cfg.Exclude, config.ClaudeDir(), cfg.Scope)
			result := mgr.AddExclude(args[0])

			if result.IsSyncPath {
				fmt.Printf("%s!%s %q is a sync path — use 'paths remove %s' instead\n",
					colorYellow, colorReset, args[0], args[0])
				return nil
			}

			if result.AlreadyExists {
				fmt.Printf("%s!%s %s is already in the exclude list\n", colorYellow, colorReset, args[0])
				return nil
			}

			cfg.Exclude = mgr.Excludes()
			if err := config.Save(cfg); err != nil {
				return err
			}

			fmt.Printf("%s✓%s Added exclude filter: %s\n", colorGreen, colorReset, args[0])
			return nil
		},
	}
}

func pathsUnexcludeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "unexclude <glob>",
		Short: "Remove a sub-path glob filter",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load()
			if err != nil {
				return err
			}

			mgr := paths.NewManager(cfg.SyncPaths, cfg.Exclude, config.ClaudeDir(), cfg.Scope)
			result := mgr.RemoveExclude(args[0])

			if result.NotFound {
				fmt.Printf("%s!%s %s not found in exclude list\n", colorYellow, colorReset, args[0])
				return nil
			}

			cfg.Exclude = mgr.Excludes()
			if err := config.Save(cfg); err != nil {
				return err
			}

			fmt.Printf("%s✓%s Removed exclude filter: %s\n", colorGreen, colorReset, args[0])
			return nil
		},
	}
}

func pathsResetCmd() *cobra.Command {
	var force bool
	cmd := &cobra.Command{
		Use:   "reset",
		Short: "Restore default sync paths and clear excludes",
		RunE: func(cmd *cobra.Command, args []string) error {
			if !force {
				var confirm bool
				prompt := &survey.Confirm{
					Message: "Reset all sync paths and filters to defaults?",
					Default: false,
				}
				if err := survey.AskOne(prompt, &confirm); err != nil || !confirm {
					fmt.Println("  Cancelled.")
					return nil
				}
			}

			cfg, err := config.Load()
			if err != nil {
				return err
			}

			cfg.SyncPaths = nil
			cfg.Exclude = nil
			if err := config.Save(cfg); err != nil {
				return err
			}

			fmt.Printf("%s✓%s Sync paths reset to defaults\n", colorGreen, colorReset)
			return nil
		},
	}
	cmd.Flags().BoolVarP(&force, "force", "f", false, "Skip confirmation")
	return cmd
}
