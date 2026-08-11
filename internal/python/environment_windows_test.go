//go:build windows

package python

import (
	"errors"
	"strings"
	"testing"
)

func TestPlatformWorkerEnvironmentUsesWindowsAPI(t *testing.T) {
	original := getWindowsDirectory
	t.Cleanup(func() { getWindowsDirectory = original })
	getWindowsDirectory = func() (string, error) { return `C:\Windows`, nil }
	entries, err := platformWorkerEnvironment()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].name != "SystemRoot" || entries[0].value != `C:\Windows` {
		t.Fatalf("platform environment = %#v", entries)
	}
}

func TestPlatformWorkerEnvironmentRejectsWindowsAPIFailure(t *testing.T) {
	original := getWindowsDirectory
	t.Cleanup(func() { getWindowsDirectory = original })
	getWindowsDirectory = func() (string, error) { return "", errors.New("injected failure") }
	if _, err := platformWorkerEnvironment(); err == nil || !strings.Contains(err.Error(), "get Windows directory") {
		t.Fatalf("platformWorkerEnvironment() error = %v", err)
	}
	getWindowsDirectory = func() (string, error) { return "relative", nil }
	if _, err := platformWorkerEnvironment(); err == nil || !strings.Contains(err.Error(), "invalid path") {
		t.Fatalf("invalid Windows directory error = %v", err)
	}
}

func TestWindowsEnvironmentIdentityIsCaseInsensitive(t *testing.T) {
	if normalizeEnvironmentKey("Path") != normalizeEnvironmentKey("PATH") {
		t.Fatal("Windows environment key normalization is case-sensitive")
	}
	first := "one"
	second := "two"
	_, err := loadWorkerEnvironment(Config{Environment: map[string]*string{"VALUE": &first, "value": &second}}, nil)
	if err == nil || !strings.Contains(err.Error(), "same variable") {
		t.Fatalf("case collision error = %v", err)
	}
}
