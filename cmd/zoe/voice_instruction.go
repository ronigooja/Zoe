package main

import (
	"context"
	"fmt"
	"time"

	"github.com/ronigooja/Zoe/internal/ops"
)

func menuVoiceInstruction(m ConfigureMenuIO) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	d, err := ops.DiscoverDeployment(ctx, opsManager(), m.Root)
	if err != nil {
		return err
	}
	raw, effective, err := d.VoiceInstruction()
	if err != nil {
		return err
	}
	mode := "custom"
	if raw == "" {
		mode = "default"
	}
	if raw == "off" {
		mode = "off (disabled)"
	}
	fmt.Fprintf(m.Out, "Current voice instruction (%s):\n%s\n", mode, effective)
	fmt.Fprintln(m.Out, "Sent verbatim as instructions in each gateway create-session request; no prefix.")
	fmt.Fprint(m.Out, "New instruction (single line; off disables; Enter restores default): ")
	text, err := inputLine(m.In)
	if err != nil {
		return err
	}
	if err := ops.WriteVoiceInstructionAtomic(d.StackEnv, text); err != nil {
		return fmt.Errorf("nothing saved: %w", err)
	}
	fmt.Fprintln(m.Out, "Voice instruction saved.")
	fmt.Fprintln(m.Out, "Apply with: zoe update")
	return nil
}
