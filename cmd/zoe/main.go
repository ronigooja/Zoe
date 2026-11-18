// Command zoe is the operator entry point for local Zoe maintenance.
// With no arguments it opens a small menu so operators do not need to
// remember sub-command names. Selected actions also have a scriptable form.
package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/ronigooja/Zoe/internal/logging"
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
	configureCLILogging()
	if len(os.Args) == 2 && os.Args[1] == "--source-revision" {
		fmt.Println(sourceRevision)
		return
	}
	if err := run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "zoe:", err)
		os.Exit(1)
	}
}

func configureCLILogging() {
	value, set := os.LookupEnv("ZOE_LOG_LEVEL")
	if _, valid := logging.ParseLevel(value); set && strings.TrimSpace(value) != "" && valid {
		logging.ConfigureFromEnv()
		return
	}
	logging.SetLevel(logging.Error)
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
	return errors.New("usage: zoe [setup|install|update|uninstall|provider configure|provider test|memory configure|memory test|key init|key rotate|status|voice configure|voice test|voice status|client-policy show|client-policy set]")
}

func menu(in io.Reader, out, errOut io.Writer) error {
	return menuWithRoot(in, out, errOut, "/etc/zoe")
}

func menuWithRoot(in io.Reader, out, errOut io.Writer, root string) error {
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
		fmt.Fprintln(out, "  6) Configure Worker model (runs tasks)")
		fmt.Fprintln(out, "  7) Test Worker model")
		fmt.Fprintln(out, "  8) Configure Register model (routes every sentence)")
		fmt.Fprintln(out, "  9) Test Register model")
		fmt.Fprintln(out, "  10) Configure Compact model (hourly conversation compaction)")
		fmt.Fprintln(out, "  11) Test Compact model")
		fmt.Fprintln(out, "  12) Configure Steward model (task routing and replies)")
		fmt.Fprintln(out, "  13) Test Steward model")
		fmt.Fprintln(out, "  24) Configure UI_Controller model (Mac screen content and input)")
		fmt.Fprintln(out, "  25) Test UI_Controller model")
		fmt.Fprintln(out, "  14) Configure Memory extraction model (24-hour batch and memory worker)")
		fmt.Fprintln(out, "  15) Test Memory extraction model")
		fmt.Fprintln(out, "Voice:")
		fmt.Fprintln(out, "  16) Configure voice gateway")
		fmt.Fprintln(out, "  17) Test voice gateway")
		fmt.Fprintln(out, "  18) Show voice gateway status")
		fmt.Fprintln(out, "Access:")
		fmt.Fprintln(out, "  20) Create instance key")
		fmt.Fprintln(out, "  21) Rotate instance key")
		fmt.Fprintln(out, "Client policy:")
		fmt.Fprintln(out, "  22) Show client policy")
		fmt.Fprintln(out, "  23) Set client policy")
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
		var actionErr error
		switch choice {
		case "6":
			actionErr = menuWorker(configureMenuIO(root, in, out, errOut))
		case "7":
			actionErr = menuTestWorker(root, out)
		case "8", "10", "12", "14", "24":
			kind := map[string]string{"8": "Register", "10": "Compact", "12": "Steward", "14": "Memory", "24": "UI_Controller"}[choice]
			actionErr = menuModel(configureMenuIO(root, in, out, errOut), kind)
		case "9", "11", "13", "25":
			kind := map[string]string{"9": "Register", "11": "Compact", "13": "Steward", "25": "UI_Controller"}[choice]
			actionErr = menuTestModel(root, out, kind)
		default:
			name, ok := map[string]string{"1": "status", "2": "setup", "3": "install", "4": "update", "5": "uninstall", "15": "memory test", "16": "voice configure", "17": "voice test", "18": "voice status", "20": "key init", "21": "key rotate", "22": "client-policy show", "23": "client-policy set"}[choice]
			if ok {
				actionErr = operations[name](nil, in, out, errOut)
				break
			}
			fmt.Fprintln(errOut, "Please choose a number from 0 to 25.")
			continue
		}
		if actionErr != nil && !errors.Is(actionErr, io.EOF) {
			fmt.Fprintln(errOut, "Error:", actionErr)
		}
		fmt.Fprintln(out, "Press Enter to return to the menu")
		if _, err := inputLine(in); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
	}
}
