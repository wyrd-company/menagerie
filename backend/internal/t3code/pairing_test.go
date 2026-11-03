package t3code

import (
	"errors"
	"strings"
	"testing"
)

func TestParseConfigReadsTheFeaturesRecord(t *testing.T) {
	// The file as the Feature's install.sh writes it.
	got, err := parseConfig(strings.NewReader("T3CODE_SERVER_USER=sample\nT3CODE_SERVER_HOME=/home/sample\nT3CODE_SERVER_ARCH=x64\nT3CODE_SERVER_PORT=3773\nT3CODE_SERVER_HOST=0.0.0.0\nT3CODE_SERVER_MODE=''\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got.user != "sample" || got.home != "/home/sample" {
		t.Errorf("got %+v", got)
	}
}

func TestParseConfigRefusesWhatOnlyAShellCouldRead(t *testing.T) {
	for name, config := range map[string]string{
		"empty":        "",
		"no user":      "T3CODE_SERVER_HOME=/home/sample\n",
		"no home":      "T3CODE_SERVER_USER=sample\n",
		"quoted user":  "T3CODE_SERVER_USER=sam\\ ple\nT3CODE_SERVER_HOME=/home/sample\n",
		"relative":     "T3CODE_SERVER_USER=sample\nT3CODE_SERVER_HOME=home/sample\n",
		"user colon":   "T3CODE_SERVER_USER=sample:0\nT3CODE_SERVER_HOME=/home/sample\n",
		"quoted home":  "T3CODE_SERVER_USER=sample\nT3CODE_SERVER_HOME=/home/$(id)\n",
		"too long":     "T3CODE_SERVER_USER=" + strings.Repeat("a", 1<<17) + "\nT3CODE_SERVER_HOME=/home/sample\n",
		"leading dash": "T3CODE_SERVER_USER=-sample\nT3CODE_SERVER_HOME=/home/sample\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parseConfig(strings.NewReader(config)); !errors.Is(err, ErrNotInstalled) {
				t.Errorf("err = %v", err)
			}
		})
	}
}

func TestCommandUsesTheServersDataDirectoryAndJSON(t *testing.T) {
	got := strings.Join(command(service{user: "sample", home: "/home/sample"}), " ")
	want := "/usr/local/bin/t3 auth pairing create --base-dir /home/sample/.t3 --label menagerie --json"
	if got != want {
		t.Errorf("command = %q", got)
	}
}

func TestParseTokenReadsTheCommandsJSON(t *testing.T) {
	// The shape of `t3 auth pairing create --json`.
	got, err := parseToken([]byte(`{
  "id": "00000000-0000-0000-0000-000000000000",
  "credential": "SAMPLETOKEN123",
  "label": "menagerie",
  "scopes": ["orchestration:read"],
  "expiresAt": "2030-01-02T03:04:05.777Z"
}
`))
	if err != nil {
		t.Fatal(err)
	}
	if got.Token != "SAMPLETOKEN123" || got.ExpiresAt != "2030-01-02T03:04:05.777Z" {
		t.Errorf("got %+v", got)
	}
}

func TestParseTokenFailsWithoutQuotingTheOutput(t *testing.T) {
	const secret = "SYNTHETICSECRET"
	for name, output := range map[string]string{
		"empty":         "",
		"not JSON":      secret,
		"JSON then not": `{"credential":"` + secret + `"} ` + secret,
		"no credential": `{"expiresAt":"2030-01-02T03:04:05Z"}`,
		"no expiry":     `{"credential":"` + secret + `"}`,
		"bad expiry":    `{"credential":"` + secret + `","expiresAt":"` + secret + `"}`,
		"wrong type":    `{"credential":["` + secret + `"],"expiresAt":"2030-01-02T03:04:05Z"}`,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := parseToken([]byte(output))
			if !errors.Is(err, ErrFailed) {
				t.Fatalf("err = %v", err)
			}
			if strings.Contains(err.Error(), secret) {
				t.Errorf("error quotes the output: %v", err)
			}
		})
	}
}

func TestLimitedBufferFailsTheWriteThatPassesItsLimit(t *testing.T) {
	b := &limitedBuffer{limit: 4}
	if _, err := b.Write([]byte("abcd")); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Write([]byte("e")); !errors.Is(err, errOutputTooLong) {
		t.Errorf("err = %v", err)
	}
}
