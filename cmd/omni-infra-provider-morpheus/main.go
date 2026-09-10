// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

// Package main runs the Morpheus Omni infrastructure provider.
package main

import (
	"context"
	_ "embed"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"unicode"

	"github.com/siderolabs/omni/client/pkg/client"
	"github.com/siderolabs/omni/client/pkg/infra"
	"github.com/spf13/cobra"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	"github.com/kreove/omni-infra-provider-morpheus/internal/pkg/provider"
	"github.com/kreove/omni-infra-provider-morpheus/internal/pkg/provider/data"
	"github.com/kreove/omni-infra-provider-morpheus/internal/pkg/provider/meta"
)

//go:embed data/icon.svg
var icon []byte

// version is stamped at build time with -ldflags "-X main.version=...".
// It stays "dev" for local builds.
var version = "dev"

var cfg struct {
	omniAPIEndpoint        string
	serviceAccountKey      string
	providerName           string
	providerDescription    string
	morpheusEndpoint       string
	morpheusToken          string
	morpheusUsername       string
	morpheusPassword       string
	morpheusInsecure       bool
	omniInsecureSkipVerify bool
}

var rootCmd = &cobra.Command{
	Use:          "omni-infra-provider-morpheus",
	Short:        "Morpheus Omni infrastructure provider",
	Long:         "Connects to Sidero Omni as an infrastructure provider and manages Talos VMs on HPE Morpheus VM Essentials (MVM).",
	Version:      version,
	SilenceUsage: true,
	RunE: func(cmd *cobra.Command, _ []string) error {
		loggerConfig := zap.NewProductionConfig()

		logger, err := loggerConfig.Build(zap.AddStacktrace(zapcore.ErrorLevel))
		if err != nil {
			return fmt.Errorf("failed to create logger: %w", err)
		}

		// Values arrive from .env files, Kubernetes Secrets, and shell
		// exports, all of which pick up stray whitespace easily.
		normalizeConfig()

		if cfg.omniAPIEndpoint == "" {
			return fmt.Errorf("omni-api-endpoint is required")
		}

		if cfg.morpheusEndpoint == "" {
			return fmt.Errorf("morpheus-endpoint is required")
		}

		if err = validateServiceAccountKey(cfg.serviceAccountKey); err != nil {
			return err
		}

		morpheusClient, err := provider.NewClient(provider.ClientConfig{
			Endpoint: cfg.morpheusEndpoint,
			Token:    cfg.morpheusToken,
			Username: cfg.morpheusUsername,
			Password: cfg.morpheusPassword,
			Insecure: cfg.morpheusInsecure,
		})
		if err != nil {
			return err
		}

		provisioner := provider.NewProvisioner(morpheusClient)

		infrastructureProvider, err := infra.NewProvider(
			meta.ProviderID,
			provisioner,
			infra.ProviderConfig{
				Name:        cfg.providerName,
				Description: cfg.providerDescription,
				Icon:        base64.RawStdEncoding.EncodeToString(icon),
				Schema:      string(data.Schema),
			},
		)
		if err != nil {
			return fmt.Errorf("failed to create infrastructure provider: %w", err)
		}

		clientOptions := []client.Option{
			client.WithInsecureSkipTLSVerify(cfg.omniInsecureSkipVerify),
		}
		if cfg.serviceAccountKey != "" {
			clientOptions = append(clientOptions, client.WithServiceAccount(cfg.serviceAccountKey))
		}

		logger.Info(
			"starting Morpheus infrastructure provider",
			zap.String("version", version),
			zap.String("provider_id", meta.ProviderID),
			zap.String("morpheus_endpoint", cfg.morpheusEndpoint),
		)

		return infrastructureProvider.Run(
			cmd.Context(),
			logger,
			infra.WithOmniEndpoint(cfg.omniAPIEndpoint),
			infra.WithClientOptions(clientOptions...),
			infra.WithEncodeRequestIDsIntoTokens(),
		)
	},
}

func main() {
	if err := app(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func app() error {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGHUP, syscall.SIGTERM)
	defer cancel()

	return rootCmd.ExecuteContext(ctx)
}

// normalizeConfig cleans up configuration values that commonly arrive with
// stray whitespace from .env files, Kubernetes Secrets, or shell exports.
//
// The service account key gets stricter treatment than a trim. Omni decodes it
// with base64.StdEncoding, which ignores \r and \n but rejects spaces and tabs
// outright, failing with an opaque "illegal base64 data at input byte N". A key
// that picked up a stray space -- from a wrapped terminal copy, an editor, or a
// hand-edited secret -- therefore crash-loops the provider for a reason the
// error does not reveal. Stripping whitespace is safe, since none of it ever
// appears in valid base64.
func normalizeConfig() {
	cfg.serviceAccountKey = stripWhitespace(cfg.serviceAccountKey)

	cfg.omniAPIEndpoint = strings.TrimSpace(cfg.omniAPIEndpoint)
	cfg.morpheusEndpoint = strings.TrimSpace(cfg.morpheusEndpoint)
	cfg.morpheusToken = strings.TrimSpace(cfg.morpheusToken)
	cfg.morpheusUsername = strings.TrimSpace(cfg.morpheusUsername)
}

func stripWhitespace(value string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return -1
		}

		return r
	}, value)
}

// validateServiceAccountKey fails fast with an actionable message instead of
// letting the Omni client report a bare byte offset.
func validateServiceAccountKey(key string) error {
	if key == "" {
		return nil
	}

	if _, err := base64.StdEncoding.DecodeString(key); err != nil {
		return fmt.Errorf(
			"OMNI_SERVICE_ACCOUNT_KEY is not valid base64: %w\n%s\n"+
				"It must be the single-line value printed by 'omnictl infraprovider create %s'. "+
				"The key is only ever displayed at creation, so if you no longer have a clean copy, "+
				"issue a new one with 'omnictl infraprovider renewkey %s'.",
			err, describeBase64Fault(key, err), meta.ProviderID, meta.ProviderID,
		)
	}

	return nil
}

// describeBase64Fault turns a bare byte offset into a description of the actual
// offending character. Base64 decode errors are otherwise near-impossible to
// act on, because the value is a long opaque blob that cannot simply be eyeballed.
func describeBase64Fault(key string, err error) string {
	var corrupt base64.CorruptInputError
	if !errors.As(err, &corrupt) {
		return "The value does not decode as base64. Check that it was copied in full."
	}

	offset := int(corrupt)
	if offset < 0 || offset >= len(key) {
		return fmt.Sprintf(
			"The value is %d characters long and appears to be truncated. Check that it was copied in full.",
			len(key),
		)
	}

	bad := key[offset]

	switch {
	case bad == '=':
		// Confirmed in the field: a duplicated trailing '=' from a copy/paste.
		return fmt.Sprintf(
			"Character %d of %d is '=', which is base64 padding appearing where it is not valid. "+
				"This almost always means an extra '=' was appended when the value was copied. "+
				"A key ends with either one '=' or two, never three, and its total length is a "+
				"multiple of 4 (this one is %d). Try removing the trailing '='.",
			offset+1, len(key), len(key),
		)
	case bad == ' ' || bad == '\t':
		return fmt.Sprintf(
			"Character %d of %d is whitespace. The provider strips whitespace before decoding, so "+
				"seeing this means it was re-introduced elsewhere, e.g. by shell quoting.",
			offset+1, len(key),
		)
	case bad == '"' || bad == '\'':
		return fmt.Sprintf(
			"Character %d of %d is a quote. Quotes around the value in a .env file should not be "+
				"included in the value itself.",
			offset+1, len(key),
		)
	default:
		return fmt.Sprintf(
			"Character %d of %d is %q, which is not part of the base64 alphabet (A-Z a-z 0-9 + / =).",
			offset+1, len(key), string(bad),
		)
	}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}

	return ""
}

func init() {
	rootCmd.Flags().StringVar(
		&cfg.omniAPIEndpoint,
		"omni-api-endpoint",
		os.Getenv("OMNI_ENDPOINT"),
		"Omni API endpoint (defaults to OMNI_ENDPOINT)",
	)
	rootCmd.Flags().StringVar(
		&meta.ProviderID,
		"id",
		meta.ProviderID,
		"provider ID registered in Omni",
	)
	rootCmd.Flags().StringVar(
		&cfg.serviceAccountKey,
		"omni-service-account-key",
		os.Getenv("OMNI_SERVICE_ACCOUNT_KEY"),
		"Omni service account key (defaults to OMNI_SERVICE_ACCOUNT_KEY)",
	)
	rootCmd.Flags().StringVar(&cfg.providerName, "provider-name", "Morpheus", "provider name shown in Omni")
	rootCmd.Flags().StringVar(
		&cfg.providerDescription,
		"provider-description",
		"HPE Morpheus VM Essentials infrastructure provider (alpha)",
		"provider description shown in Omni",
	)
	rootCmd.Flags().StringVar(
		&cfg.morpheusEndpoint,
		"morpheus-endpoint",
		firstNonEmpty(os.Getenv("MORPHEUS_ENDPOINT"), os.Getenv("MORPHEUS_URL")),
		"Morpheus appliance base URL, e.g. https://morpheus.example.com (defaults to MORPHEUS_ENDPOINT, then MORPHEUS_URL)",
	)
	rootCmd.Flags().StringVar(
		&cfg.morpheusToken,
		"morpheus-token",
		os.Getenv("MORPHEUS_TOKEN"),
		"Morpheus API token (defaults to MORPHEUS_TOKEN)",
	)
	rootCmd.Flags().StringVar(
		&cfg.morpheusUsername,
		"morpheus-username",
		os.Getenv("MORPHEUS_USERNAME"),
		"Morpheus username when token authentication is not used",
	)
	rootCmd.Flags().StringVar(
		&cfg.morpheusPassword,
		"morpheus-password",
		os.Getenv("MORPHEUS_PASSWORD"),
		"Morpheus password when token authentication is not used",
	)
	rootCmd.Flags().BoolVar(
		&cfg.morpheusInsecure,
		"morpheus-insecure-skip-verify",
		false,
		"skip Morpheus TLS certificate verification",
	)
	rootCmd.Flags().BoolVar(
		&cfg.omniInsecureSkipVerify,
		"insecure-skip-verify",
		false,
		"skip Omni TLS verification",
	)
}
