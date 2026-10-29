package config

import (
	"strings"
	"testing"
)

func TestShortShareSecretIsRefused(t *testing.T) {
	cfg := Default()
	cfg.General.BaseFolderFiles = t.TempDir()
	cfg.HTTP.ShareLinkSecret = "short"
	if err := cfg.validateHTTP(); err == nil || !strings.Contains(err.Error(), "shareLinkSecret") {
		t.Errorf("validateHTTP = %v", err)
	}
	cfg.HTTP.ShareLinkSecret = strings.Repeat("x", MinShareSecret)
	if err := cfg.validateHTTP(); err != nil {
		t.Errorf("validateHTTP = %v", err)
	}
}
