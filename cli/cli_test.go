package cli

import (
	"bytes"
	"context"
	"os"
	"testing"

	ucli "github.com/urfave/cli/v2"
)

// unsetenv unsets the variables for the rest of the test, and sets them
// back as they were after it.
func unsetenv(t *testing.T, names ...string) {
	t.Helper()
	for _, name := range names {
		t.Setenv(name, "")
		if err := os.Unsetenv(name); err != nil {
			t.Fatal(err)
		}
	}
}

// writeFile writes content to path, mode 0600.
func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// run runs app with args after the program's name, and returns what it
// wrote to its two writers and its error.
func run(t *testing.T, app *ucli.App, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	var out, errOut bytes.Buffer
	app.Writer = &out
	app.ErrWriter = &errOut
	err = app.RunContext(context.Background(), append([]string{"kosh"}, args...))
	return out.String(), errOut.String(), err
}

func TestVersion(t *testing.T) {
	old := Version
	Version = "v1.2.3-test"
	t.Cleanup(func() { Version = old })

	out, _, err := run(t, App(), "version")
	if err != nil {
		t.Fatal(err)
	}
	if out != "v1.2.3-test\n" {
		t.Errorf("printed %q, want the version and a line feed", out)
	}
}
