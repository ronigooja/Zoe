// Command zoe is the operator entry point for local Zoe maintenance.
// With no arguments it opens a small menu so operators do not need to
// remember sub-command names. Every menu action also has a scriptable form.
package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

// sourceRevision is set by release builds and is used by the host updater to
// prove that the operator binary belongs to the reviewed server release.
var sourceRevision = "development"

type operation func(args []string, in io.Reader, out, errOut io.Writer) error

// Operations are registered by the command implementations. Keeping the
// dispatch layer independent lets setup, provider and key flows share one
// menu while remaining usable from scripts.
var operations = map[string]operation{
	"setup":              unavailable("setup"),
	"configure":          unavailable("configure"),
	"triage configure":   unavailable("triage configure"),
	"triage test":        unavailable("triage test"),
	"provider configure": unavailable("provider configure"),
	"provider test":      unavailable("provider test"),
	"memory configure":   unavailable("memory configure"),
	"memory test":        unavailable("memory test"),
	"key init":           unavailable("key init"),
	"key rotate":         unavailable("key rotate"),
	"status":             unavailable("status"),
	"voice configure":    unavailable("voice configure"),
	"voice test":         unavailable("voice test"),
	"voice status":       unavailable("voice status"),
	"client-policy show": unavailable("client-policy show"),
	"client-policy set":  unavailable("client-policy set"),
	"install":            unavailable("install"),
	"update":             unavailable("update"),
	"uninstall":          unavailable("uninstall"),
}

func unavailable(name string) operation {
	return func(_ []string, _ io.Reader, _ io.Writer, _ io.Writer) error {
		return fmt.Errorf("zoe %s is unavailable in this build", name)
	}
}

// RegisterOperation replaces one operation. It is intentionally small so
// command implementations can be assembled in separate files without
// duplicating argument or menu handling.
func RegisterOperation(name string, fn func([]string, io.Reader, io.Writer, io.Writer) error) error {
	name = normalizeCommand(name)
	if _, ok := operations[name]; !ok {
		return fmt.Errorf("unknown zoe operation %q", name)
	}
	if fn == nil {
		return errors.New("zoe operation cannot be nil")
	}
	operations[name] = fn
	return nil
}

func main() {
	if len(os.Args) == 2 && os.Args[1] == "--source-revision" {
		fmt.Println(sourceRevision)
		return
	}
	if err := run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "zoe:", err)
		os.Exit(1)
	}
}

func run(args []string, in io.Reader, out, errOut io.Writer) error {
	in = sharedInput(in)
	if len(args) == 0 {
		return menu(in, out, errOut)
	}
	name, rest, err := parseCommand(args)
	if err != nil {
		return err
	}
	fn, ok := operations[name]
	if !ok {
		return usageError()
	}
	return fn(rest, in, out, errOut)
}

func parseCommand(args []string) (string, []string, error) {
	if len(args) == 0 {
		return "", nil, errors.New("missing command")
	}
	if len(args) >= 2 {
		name := normalizeCommand(args[0] + " " + args[1])
		if _, ok := operations[name]; ok {
			return name, args[2:], nil
		}
	}
	name := normalizeCommand(args[0])
	if _, ok := operations[name]; ok {
		return name, args[1:], nil
	}
	return "", nil, usageError()
}

func normalizeCommand(value string) string {
	return strings.Join(strings.Fields(strings.ToLower(value)), " ")
}

func usageError() error {
	return errors.New("usage: zoe [setup|configure|install|update|uninstall|provider configure|provider test|memory configure|memory test|triage configure|triage test|key init|key rotate|status|voice configure|voice test|voice status|client-policy show|client-policy set]")
}

func menu(in io.Reader, out, errOut io.Writer) error {
	in = sharedInput(in)
	for {
		fmt.Fprintln(out)
		fmt.Fprintln(out, "Zoe maintenance")
		fmt.Fprintln(out, "Overview:")
		fmt.Fprintln(out, "  1) Show status")
		fmt.Fprintln(out, "  2) First-time setup")
		fmt.Fprintln(out, "System:")
		fmt.Fprintln(out, "  3) Install host")
		fmt.Fprintln(out, "  4) Update host")
		fmt.Fprintln(out, "  5) Uninstall host")
		fmt.Fprintln(out, "Models:")
		fmt.Fprintln(out, "  6) Configure task model")
		fmt.Fprintln(out, "  7) Test task model")
		fmt.Fprintln(out, "  8) Configure memory model")
		fmt.Fprintln(out, "  9) Test memory model")
		fmt.Fprintln(out, "Voice:")
		fmt.Fprintln(out, "  10) Configure voice gateway")
		fmt.Fprintln(out, "  11) Test voice gateway")
		fmt.Fprintln(out, "  12) Show voice gateway status")
		fmt.Fprintln(out, "Access:")
		fmt.Fprintln(out, "  13) Create instance key")
		fmt.Fprintln(out, "  14) Rotate instance key")
		fmt.Fprintln(out, "Client policy:")
		fmt.Fprintln(out, "  15) Show client policy")
		fmt.Fprintln(out, "  16) Set client policy")
		fmt.Fprintln(out, "  0) Exit")
		fmt.Fprint(out, "Select an option: ")
		line, err := inputLine(in)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		choice := strings.TrimSpace(line)
		if choice == "0" || strings.EqualFold(choice, "q") || strings.EqualFold(choice, "quit") || strings.EqualFold(choice, "exit") {
			return nil
		}
		name, ok := map[string]string{"1": "status", "2": "setup", "3": "install", "4": "update", "5": "uninstall", "6": "provider configure", "7": "provider test", "8": "memory configure", "9": "memory test", "10": "voice configure", "11": "voice test", "12": "voice status", "13": "key init", "14": "key rotate", "15": "client-policy show", "16": "client-policy set"}[choice]
		if !ok {
			fmt.Fprintln(errOut, "Please choose a number from 0 to 16.")
			continue
		}
		if err := operations[name](nil, in, out, errOut); err != nil {
			fmt.Fprintln(errOut, "Error:", err)
		}
	}
}
