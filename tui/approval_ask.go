package main

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

// Decision request card (kind=ask, daemon goal_ask.go): the goal's
// question with a vertical selection —
//
//	stop and wait          (the preselection: deny, no message)
//	<option 1..n>          (allow, msg = the option text)
//	other (type an answer) (opens a text box; allow, msg = what you typed)
//
// It sits in the same oldest-first queue as tool approvals (shared i/N).
// Like the tool card there is no one-key answer: the selection moves with
// ↑↓/tab and only enter (or the text box's enter) sends.

func newAskAnswerInput() textinput.Model {
	ti := newDenyMsgInput()
	ti.Placeholder = "your answer"
	return ti
}

func (m Model) handleAskCardKey(a approvalItem, msg tea.KeyMsg) (Model, tea.Cmd) {
	c := &m.approvalCard
	n := a.askRows()
	if c.msgMode {
		switch msg.String() {
		case "esc":
			// Back to the selection, and back to the safe preselection.
			c.msgMode, c.allow, c.askIdx = false, false, 0
			return m, nil
		case "enter":
			txt := strings.TrimSpace(c.msg.Value())
			if c.allow && txt == "" {
				return m, nil // an answer needs text; esc goes back
			}
			return m.answerApproval(a, c.allow, txt)
		}
		var cmd tea.Cmd
		c.msg, cmd = c.msg.Update(msg)
		return m, cmd
	}
	switch msg.String() {
	case "enter":
		if c.inline && !c.shownAt.IsZero() && approvalNow().Sub(c.shownAt) < approvalEnterGuard {
			return m, nil // an enter already in flight for the prompt
		}
		switch i := c.askIdx; {
		case i <= 0 || i >= n:
			return m.answerApproval(a, false, "")
		case i <= len(a.options):
			return m.answerApproval(a, true, a.options[i-1])
		default:
			c.msgMode, c.allow = true, true
			c.msg = newAskAnswerInput()
		}
	case "down", "tab":
		c.askIdx = (c.askIdx + 1) % n
	case "up", "shift+tab":
		c.askIdx = (c.askIdx + n - 1) % n
	case "m", "M":
		// stop and wait, with a note for the goal
		c.msgMode, c.allow = true, false
		c.msg = newDenyMsgInput()
	case "alt+a":
		if c.inline {
			c.shownAt = time.Time{}
			return m, nil
		}
		m.approvalCard = approvalCardState{}
	case "q":
		if !c.inline {
			m.approvalCard = approvalCardState{}
		}
	case "esc", "ctrl+c":
		if c.inline {
			m.approvalCard = approvalCardState{}
			m.enterTree()
			m.resizeViewport()
			m.refreshLog()
			return m, nil
		}
		m.approvalCard = approvalCardState{}
	}
	return m, nil
}

// askCardRows is the ask card's content at inner width w (shared by the
// modal and the inline card): title (+ i/N), origin, the question (capped
// at bodyMax lines), the selection rows, the hint.
func (m Model) askCardRows(a approvalItem, w, bodyMax int) []string {
	yel := lipgloss.NewStyle().Foreground(cYellow).Bold(true)
	gray := lipgloss.NewStyle().Foreground(cGray)
	sel := lipgloss.NewStyle().Foreground(cFgInv).Background(cAmber).Bold(true)
	title := "decision needed"
	if pos, n := m.approvalQueuePos(a); n > 1 {
		q := fmt.Sprintf("%d/%d", pos, n)
		if pad := w - ansi.StringWidth(title) - ansi.StringWidth(q); pad >= 1 {
			title += strings.Repeat(" ", pad) + q
		} else {
			title += " " + q
		}
	}
	var rows []string
	for _, l := range wrapCells(title, w) {
		rows = append(rows, yel.Render(l))
	}
	for _, l := range wrapCells(a.group+":"+sessionDisplay(a.session), w) {
		rows = append(rows, gray.Render(l))
	}
	rows = append(rows, "")
	q := wrapCells(a.summary, w)
	if budget := max(3, bodyMax); len(q) > budget {
		more := len(q) - budget + 1
		q = append(q[:budget-1], gray.Render(fmt.Sprintf("… %d more lines", more)))
	}
	rows = append(rows, q...)
	rows = append(rows, "", yel.Render(gl("⏱ ", "")+"no timeout · the goal waits"), "")
	c := m.approvalCard
	if c.msgMode {
		if c.allow {
			rows = append(rows, "your answer:", c.msg.View(), "", gray.Render("enter send · esc back"))
		} else {
			rows = append(rows, "stop and wait, note:", c.msg.View(), "", gray.Render("enter send · esc back"))
		}
		return rows
	}
	labels := append([]string{"stop and wait"}, a.options...)
	labels = append(labels, "other (type an answer)")
	for i, l := range labels {
		mark := "  "
		if i == c.askIdx {
			mark = gl("› ", "> ")
		}
		for j, ln := range wrapCells(l, max(4, w-2)) {
			p := mark
			if j > 0 {
				p = "  "
			}
			if i == c.askIdx {
				rows = append(rows, sel.Render(p+ln))
			} else {
				rows = append(rows, p+ln)
			}
		}
	}
	rows = append(rows, "")
	parts := []string{gl("↑↓", "up/dn") + " select", gl("⏎", "enter") + " send", "m stop+note", "esc later"}
	cur := ""
	for _, p := range parts {
		switch {
		case cur == "":
			cur = p
		case ansi.StringWidth(cur+" · "+p) <= w:
			cur += " · " + p
		default:
			rows = append(rows, gray.Render(cur))
			cur = p
		}
	}
	return append(rows, gray.Render(cur))
}
