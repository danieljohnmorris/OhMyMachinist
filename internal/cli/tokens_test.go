package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/owainlewis/machinist/internal/controlplane"
)

func writeTokenStoreConfig(t *testing.T) (string, string) {
	t.Helper()
	directory := t.TempDir()
	if err := os.WriteFile(filepath.Join(directory, "worker.token"), []byte("secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	database := filepath.Join(directory, "machinist.db")
	configPath := filepath.Join(directory, "config.toml")
	body := "[server]\n" +
		"database = " + strconv.Quote(filepath.ToSlash(database)) + "\n" +
		"worker_token_file = \"worker.token\"\n"
	if err := os.WriteFile(configPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return configPath, database
}

func openTestTokenStore(t *testing.T, database string) *controlplane.Store {
	t.Helper()
	store, err := controlplane.OpenStore(database)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

func runTokensCommand(t *testing.T, configPath string, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	exitCode := Execute(t.Context(), append([]string{"tokens", "--config=" + configPath}, args...),
		strings.NewReader(""), &stdout, &stderr, "test")
	return exitCode, stdout.String(), stderr.String()
}

func TestTokensCreatePrintsSecretOnce(t *testing.T) {
	configPath, database := writeTokenStoreConfig(t)
	exitCode, stdout, stderr := runTokensCommand(t, configPath, "create", "--name=cron", "--scope=write")
	if exitCode != 0 {
		t.Fatalf("exit code = %d, stderr = %q", exitCode, stderr)
	}
	secret := strings.TrimSuffix(stdout, "\n")
	if !strings.HasPrefix(secret, "mmt_") || strings.Contains(secret, "\n") {
		t.Fatalf("stdout = %q, want a single mmt_ secret line", stdout)
	}
	if !strings.Contains(stderr, "created token") || !strings.Contains(stderr, "not shown again") {
		t.Fatalf("stderr = %q, want creation notice", stderr)
	}
	if strings.Contains(stderr, secret) {
		t.Fatalf("stderr = %q leaks the secret", stderr)
	}
	store := openTestTokenStore(t, database)
	identity, scope, ok := store.AuthenticateToken(t.Context(), secret)
	if !ok || identity.Name != "cron" || scope != controlplane.TokenScopeWrite {
		t.Fatalf("AuthenticateToken = (%#v, %q, %v), want cron/write", identity, scope, ok)
	}
}

func TestTokensCreateRejectsInvalidScope(t *testing.T) {
	configPath, database := writeTokenStoreConfig(t)
	exitCode, stdout, stderr := runTokensCommand(t, configPath, "create", "--name=cron", "--scope=admin")
	if exitCode != 2 || !strings.Contains(stderr, `scope must be "read" or "write"`) {
		t.Fatalf("exit code = %d, stderr = %q, want scope rejection", exitCode, stderr)
	}
	if stdout != "" {
		t.Fatalf("stdout = %q, want no secret on failure", stdout)
	}
	// RunE failed after the store opened, so close-on-error already ran; the
	// database must be usable and must not hold the rejected token.
	store := openTestTokenStore(t, database)
	tokens, err := store.ListTokens(t.Context())
	if err != nil || len(tokens) != 0 {
		t.Fatalf("invalid scope created tokens: %v, %#v", err, tokens)
	}
}

func TestTokensListAndRevoke(t *testing.T) {
	configPath, database := writeTokenStoreConfig(t)
	var secrets []string
	for _, creation := range []struct{ name, scope string }{{"ci", "write"}, {"dashboard", "read"}} {
		exitCode, stdout, stderr := runTokensCommand(t, configPath, "create", "--name="+creation.name, "--scope="+creation.scope)
		if exitCode != 0 {
			t.Fatalf("create %s: exit code = %d, stderr = %q", creation.name, exitCode, stderr)
		}
		secrets = append(secrets, strings.TrimSpace(stdout))
	}

	exitCode, stdout, stderr := runTokensCommand(t, configPath, "list")
	if exitCode != 0 {
		t.Fatalf("list: exit code = %d, stderr = %q", exitCode, stderr)
	}
	if strings.Contains(stdout, "mmt_") {
		t.Fatalf("list = %q, leaked a secret", stdout)
	}
	lines := strings.Split(strings.TrimSuffix(stdout, "\n"), "\n")
	if len(lines) != 2 || !strings.Contains(stdout, "\twrite\tci\n") || !strings.Contains(stdout, "\tread\tdashboard\n") {
		t.Fatalf("list = %q, want both tokens with scope and name", stdout)
	}
	var revokedID string
	for _, line := range lines {
		if strings.HasSuffix(line, "\tread\tdashboard") {
			revokedID = strings.Split(line, "\t")[0]
		}
	}

	exitCode, stdout, stderr = runTokensCommand(t, configPath, "revoke", revokedID)
	if exitCode != 0 || !strings.Contains(stderr, "revoked token "+revokedID) {
		t.Fatalf("revoke: exit code = %d, stderr = %q", exitCode, stderr)
	}

	exitCode, stdout, _ = runTokensCommand(t, configPath, "list")
	if exitCode != 0 {
		t.Fatalf("list after revoke: exit code = %d", exitCode)
	}
	if lines = strings.Split(strings.TrimSuffix(stdout, "\n"), "\n"); len(lines) != 2 || !strings.Contains(stdout, "\tread\tdashboard revoked") {
		t.Fatalf("list after revoke = %q, want the revoked marker", stdout)
	}

	store := openTestTokenStore(t, database)
	if _, _, ok := store.AuthenticateToken(t.Context(), secrets[1]); ok {
		t.Fatal("revoked token still authenticates")
	}
	if _, _, ok := store.AuthenticateToken(t.Context(), secrets[0]); !ok {
		t.Fatal("unrevoked token stopped authenticating")
	}

	exitCode, _, stderr = runTokensCommand(t, configPath, "revoke", "token_doesnotexist")
	if exitCode != 2 || !strings.Contains(stderr, "revoke token token_doesnotexist:") {
		t.Fatalf("revoke unknown: exit code = %d, stderr = %q, want wrapped error", exitCode, stderr)
	}
}
