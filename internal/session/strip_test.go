package session

import "testing"

func TestStripInjectedContext(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"plain", "你好", "你好"},
		{"env whole", "<environment_context> <cwd>C:/x</cwd> <shell>powershell</shell> </environment_context>", ""},
		{"env then real same msg", "<environment_context> <cwd>C:/x</cwd> </environment_context>\n帮我分析这个仓库", "帮我分析这个仓库"},
		{"env only then next user real", "<environment_context> <cwd>C:/x</cwd> </environment_context>", ""},
		{"agents instructions block", "# AGENTS.md instructions for C:/data/github/multi2api-panel\n<INSTRUCTIONS>\n# AGENTS.md\n...</INSTRUCTIONS>", ""},
		{"nested env inside instructions", "<INSTRUCTIONS>\n<environment_context> <cwd>C:/x</cwd>\n</environment_context>\n</INSTRUCTIONS>", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := stripInjectedContext(c.in); got != c.want {
				t.Errorf("stripInjectedContext(%q) = %q want %q", c.in, got, c.want)
			}
		})
	}
}
