package main

import (
	"path/filepath"
	"strings"
	"testing"

	caddycmd "github.com/caddyserver/caddy/v2/cmd"
	"github.com/spf13/cobra"

	"github.com/smallhoursorg/motel/box"
	"github.com/smallhoursorg/motel/liveswap"
	"github.com/smallhoursorg/motel/notify"
)

// The box handler names a push's secrets/_box/ to `motel validate` by a
// variable notify reads: neither module imports the other, so the name
// is written in each and held equal here, where both are linked.
func TestNotifyPushedSecretsEnvAgrees(t *testing.T) {
	if box.NotifyPushedSecretsEnv != notify.PushedSecretsEnv {
		t.Fatalf("box names %q, notify reads %q", box.NotifyPushedSecretsEnv, notify.PushedSecretsEnv)
	}
}

// The box writes its own secrets to the file notify reads by default,
// in the directory whose files liveswap withholds NOTIFY_ variables
// from: each module restates the path, held equal here.
func TestBoxSecretsPathsAgree(t *testing.T) {
	if box.SecretsDir != liveswap.BoxSecretsDir {
		t.Errorf("box writes env files in %q, liveswap withholds from %q", box.SecretsDir, liveswap.BoxSecretsDir)
	}
	if box.BoxSecretsFile != notify.DefaultSecretsFile {
		t.Errorf("box writes its own secrets to %q, notify reads %q", box.BoxSecretsFile, notify.DefaultSecretsFile)
	}
	if filepath.Dir(notify.DefaultSecretsFile) != liveswap.BoxSecretsDir {
		t.Errorf("notify's file %q is outside %q", notify.DefaultSecretsFile, liveswap.BoxSecretsDir)
	}
}

// motel notify and motel metrics are hidden, run nothing, and fail
// with the new spelling; motel monitor holds the commands.
func TestTheOldCommandsSayTheNewSpelling(t *testing.T) {
	for _, c := range []struct {
		name string
		args []string
		want string
	}{
		{"notify", []string{"setup"}, "motel notify is now motel monitor: motel monitor setup"},
		{"notify", []string{"send", "--source", "cron"}, "motel monitor send --source cron"},
		{"notify", []string{"send", "--summary", "the nightly job failed", "--detail", "who=it's"}, `motel monitor send --summary 'the nightly job failed' --detail 'who=it'\''s'`},
		{"notify", []string{"send", "--summary", ""}, "motel monitor send --summary ''"},
		{"metrics", []string{"setup"}, "motel metrics setup is now motel monitor setup --metrics"},
	} {
		def, ok := caddycmd.Commands()[c.name]
		if !ok {
			t.Fatalf("%s is not registered", c.name)
		}
		cmd := &cobra.Command{Use: c.name}
		def.CobraFunc(cmd)
		if !cmd.Hidden {
			t.Errorf("%s is listed in the help", c.name)
		}
		if err := cmd.RunE(cmd, c.args); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("motel %s %v: %v, want %q", c.name, c.args, err, c.want)
		}
	}
	def, ok := caddycmd.Commands()["monitor"]
	if !ok {
		t.Fatal("monitor is not registered")
	}
	cmd := &cobra.Command{Use: "monitor"}
	def.CobraFunc(cmd)
	var names []string
	for _, sub := range cmd.Commands() {
		names = append(names, sub.Name())
	}
	if strings.Join(names, " ") != "send setup silence test" {
		t.Errorf("motel monitor has %v", names)
	}
	setup, _, err := cmd.Find([]string{"setup"})
	if err != nil || setup.Flags().Lookup("metrics") == nil {
		t.Errorf("motel monitor setup has no --metrics: %v", err)
	}
}
