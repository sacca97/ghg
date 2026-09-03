package agent

import (
	"fmt"
	"strings"
)

type ContextDoctorRow struct {
	Label string
	Bytes int
	Note  string
}

func FormatContextDoctor(rows []ContextDoctorRow, policy string) string {
	var b strings.Builder
	b.WriteString("Fresh-session context audit (estimated tokens)\n")
	width, total := 0, 0
	for _, row := range rows {
		if len(row.Label) > width {
			width = len(row.Label)
		}
		total += (row.Bytes + 3) / 4
	}
	for _, row := range rows {
		line := fmt.Sprintf("  %-*s %7s", width, row.Label, doctorTokens((row.Bytes+3)/4))
		if row.Note != "" {
			line += "  " + row.Note
		}
		b.WriteString(line + "\n")
	}
	fmt.Fprintf(&b, "  %-*s %7s\n", width, "TOTAL injected before you type", doctorTokens(total))
	if policy != "" {
		b.WriteString("\n" + policy + "\n")
	}
	return b.String()
}

func doctorTokens(n int) string {
	if n >= 1000 {
		return fmt.Sprintf("~%.1fk", float64(n)/1000)
	}
	return fmt.Sprintf("~%d", n)
}
