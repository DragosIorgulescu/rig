package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/BaronBonet/rig/internal/core"
	"github.com/BaronBonet/rig/internal/infrastructure"
)

// scheduleTryRequest is one `rig schedule try` run, parsed.
type scheduleTryRequest struct {
	// Task is the ID or slug of the task to run the prompt in.
	Task           string
	Prompt         string
	SessionMode    core.PromptRunSessionMode
	PermissionMode core.TaskPermissionMode
}

func newScheduleCommand(runtime commandRuntime) *cobra.Command {
	schedule := &cobra.Command{
		Use:   "schedule",
		Short: "Run stored prompts in tasks (preview)",
	}
	schedule.AddCommand(newScheduleTryCommand(runtime))
	return schedule
}

func newScheduleTryCommand(runtime commandRuntime) *cobra.Command {
	var (
		mode       string
		permission string
		promptFile string
	)
	try := &cobra.Command{
		Use:   "try <task> (<prompt> | --prompt-file <path>)",
		Short: "Run a prompt in a task once, as a scheduled run would",
		Long: `Run a prompt in a task once, the way a scheduled run does, and say whether it started.

The task is named by its ID or its slug. The prompt starts a fresh session of
the task (--mode new) or resumes its latest one (--mode same). A run never
interrupts: it is skipped, with the reason, while you are viewing the task's
window, the task is working or waiting on a permission prompt, another rig
operation is running on it, or something other than a shell or the provider is
running in its window.

A prompt that starts with "-" is read as a flag: put "--" before it, as in
rig schedule try <task> -- "- fix it", or give it with --prompt-file.`,
		Args: cobra.RangeArgs(1, 2),
		RunE: func(_ *cobra.Command, args []string) error {
			request, err := parseScheduleTry(args, mode, permission, promptFile)
			if err != nil {
				return err
			}
			if runtime.runScheduleTry == nil {
				return errors.New("command not configured")
			}
			return runtime.runScheduleTry(request)
		},
	}
	try.SetFlagErrorFunc(func(_ *cobra.Command, err error) error {
		return fmt.Errorf("%w\n(a prompt that starts with \"-\" needs \"--\" before it, or --prompt-file)", err)
	})
	try.Flags().StringVar(&mode, "mode", string(core.PromptRunSessionNew), "session to run in: new or same")
	try.Flags().StringVar(&permission, "permission", "auto", "permission mode: auto, default, accept-edits or bypass")
	try.Flags().StringVar(&promptFile, "prompt-file", "", "read the prompt from this file instead of the arguments")
	return try
}

// parseScheduleTry turns the arguments and flags of `rig schedule try` into a
// request, or says what is wrong with them.
func parseScheduleTry(args []string, mode string, permission string, promptFile string) (scheduleTryRequest, error) {
	request := scheduleTryRequest{Task: strings.TrimSpace(args[0])}
	if request.Task == "" {
		return scheduleTryRequest{}, errors.New("task is required")
	}

	switch core.PromptRunSessionMode(mode) {
	case core.PromptRunSessionNew, core.PromptRunSessionSame:
		request.SessionMode = core.PromptRunSessionMode(mode)
	default:
		return scheduleTryRequest{}, fmt.Errorf("unknown --mode %q: use new or same", mode)
	}

	switch permission {
	case "auto":
		request.PermissionMode = core.TaskPermissionModeAuto
	case "default":
		request.PermissionMode = core.TaskPermissionModeDefault
	case "accept-edits":
		request.PermissionMode = core.TaskPermissionModeAcceptEdits
	case "bypass":
		request.PermissionMode = core.TaskPermissionModeBypass
	default:
		return scheduleTryRequest{}, fmt.Errorf(
			"unknown --permission %q: use auto, default, accept-edits or bypass", permission)
	}

	switch {
	case promptFile != "" && len(args) > 1:
		return scheduleTryRequest{}, errors.New("give the prompt as an argument or with --prompt-file, not both")
	case promptFile != "":
		content, err := os.ReadFile(promptFile)
		if err != nil {
			return scheduleTryRequest{}, fmt.Errorf("read prompt file: %w", err)
		}
		request.Prompt = string(content)
	case len(args) > 1:
		request.Prompt = args[1]
	}
	if strings.TrimSpace(request.Prompt) == "" {
		return scheduleTryRequest{}, errors.New("a prompt is required: give it as an argument or with --prompt-file")
	}
	return request, nil
}

// runScheduleTry runs the request against the running task daemon, starting it
// if need be.
func runScheduleTry(stdout io.Writer, request scheduleTryRequest) error {
	if stdout == nil {
		stdout = os.Stdout
	}

	cfg, err := infrastructure.LoadConfig()
	if err != nil {
		return err
	}
	adapter, err := newClientTaskDaemon(cfg)
	if err != nil {
		return err
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	if err := adapter.EnsureRunning(ctx); err != nil {
		return err
	}
	frontend := adapter.Frontend()
	if frontend == nil {
		return errors.New("task frontend not configured")
	}
	return tryTaskPrompt(ctx, stdout, frontend, request)
}

// tryTaskPrompt finds the task the request names and runs the prompt in it,
// printing how it went. A skipped run is a result, not an error.
func tryTaskPrompt(ctx context.Context, stdout io.Writer, service core.TaskService, request scheduleTryRequest) error {
	tasks, err := service.ListTasks(ctx)
	if err != nil {
		return fmt.Errorf("list tasks: %w", err)
	}
	task, err := findScheduleTask(tasks, request.Task)
	if err != nil {
		return err
	}

	result, err := service.RunTaskPrompt(ctx, core.RunTaskPromptInput{
		TaskID:         task.ID,
		Prompt:         request.Prompt,
		SessionMode:    request.SessionMode,
		PermissionMode: request.PermissionMode,
	})
	if err != nil {
		return err
	}

	fmt.Fprintf(stdout, "Task: %s (%s)\n", task.DisplayName, task.ID)
	fmt.Fprintf(stdout, "Outcome: %s\n", result.Outcome)
	fmt.Fprintf(stdout, "Detail: %s\n", result.Detail)
	return nil
}

// findScheduleTask returns the one task whose ID or slug is exactly ref.
func findScheduleTask(tasks []*core.Task, ref string) (*core.Task, error) {
	var matches []*core.Task
	for _, task := range tasks {
		if task != nil && (task.ID == ref || task.Slug == ref) {
			matches = append(matches, task)
		}
	}
	switch len(matches) {
	case 0:
		return nil, fmt.Errorf("no task has the ID or slug %q", ref)
	case 1:
		return matches[0], nil
	default:
		ids := make([]string, 0, len(matches))
		for _, task := range matches {
			ids = append(ids, task.ID)
		}
		return nil, fmt.Errorf("%q matches several tasks (%s): use the ID of one", ref, strings.Join(ids, ", "))
	}
}
