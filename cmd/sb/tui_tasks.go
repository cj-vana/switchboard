package main

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/switchboard-code/switchboard/internal/catalog"
	"github.com/switchboard-code/switchboard/internal/delegate"
)

func cmdTasks(m *tuiModel, args string) tea.Cmd {
	parentID := m.app.loop.Session.ID()
	fields := strings.Fields(args)
	if len(fields) > 0 {
		if len(fields) != 2 || fields[0] != "cancel" {
			return noticeCmd("warn", "usage: /tasks [cancel <id>]")
		}
		var found *delegate.TaskSnapshot
		for _, task := range tasksForSession(subagentTasks, parentID) {
			if task.ID == fields[1] {
				copy := task
				found = &copy
				break
			}
		}
		if found == nil {
			return noticeCmd("warn", "no delegate task "+workspaceSanitize(fields[1])+" belongs to this session")
		}
		if err := subagentTasks.Cancel(found.ID); err != nil {
			return noticeCmd("warn", err.Error())
		}
		return noticeCmd("", "cancelling "+found.ID+" "+workspaceSanitize(found.Name)+"; other delegate tasks keep running")
	}

	m.addInfo(renderTasks(tasksForSession(subagentTasks, parentID), subagentTasks.MaxParallel(), parentID))
	return nil
}

func tasksForSession(manager *delegate.TaskManager, parentID string) []delegate.TaskSnapshot {
	var out []delegate.TaskSnapshot
	for _, task := range manager.List() {
		if task.ParentSessionID == parentID {
			out = append(out, task)
		}
	}
	return out
}

func renderTasks(tasks []delegate.TaskSnapshot, maxParallel int, parentID string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "delegate tasks · session %s · at most %d active\n", workspaceSanitize(parentID), maxParallel)
	if len(tasks) == 0 {
		b.WriteString("\n  none in this session")
		return b.String()
	}
	b.WriteString("\n  id        status      tier    cost          name\n")
	for _, task := range tasks {
		cost := "0 observed"
		if task.CostMicroUSD > 0 {
			cost = catalog.Money(task.CostMicroUSD).String()
		}
		fmt.Fprintf(&b, "  %-9s %-11s %-7s %-13s %s\n",
			workspaceSanitize(task.ID), workspaceSanitize(string(task.Status)),
			workspaceSanitize(task.Tier), cost, workspaceSanitize(task.Name))
		subsession := task.DelegateSessionID
		if subsession == "" {
			subsession = "pending"
		}
		fmt.Fprintf(&b, "    parent %s · delegate %s · %d calls\n",
			workspaceSanitize(task.ParentSessionID), workspaceSanitize(subsession), task.Calls)
		if task.Error != "" {
			fmt.Fprintf(&b, "    %s\n", workspaceSanitize(task.Error))
		}
	}
	b.WriteString("\n  /tasks cancel <id> stops one queued or running delegate only")
	return strings.TrimRight(b.String(), "\n")
}
