package middlewares

import (
	"context"
	"strings"

	wechatbot "github.com/corespeed-io/wechatbot/golang"
)

// Command binds one or more textual aliases (each a full "/foo bar" path) to a
// handler. args holds the whitespace-separated tokens following the matched
// alias.
type Command struct {
	Aliases []string
	ArgHint string // e.g. "<name>"; shown in usage/help. "" when the command takes no args.
	MinArgs int
	Desc    string // shown in the generated help listing
	Hidden  bool   // omit from Help (e.g. namespace catch-alls)
	Run     func(ctx context.Context, msg *wechatbot.IncomingMessage, args []string)
}

// match reports whether text invokes this command, returning the trailing
// argument tokens when it does.
func (c Command) match(text string) ([]string, bool) {
	for _, alias := range c.Aliases {
		if text == alias {
			return nil, true
		}
		if rest, ok := strings.CutPrefix(text, alias+" "); ok {
			return strings.Fields(rest), true
		}
	}
	return nil, false
}

// Usage returns the canonical invocation form, e.g. "/session use <编号>".
func (c Command) Usage() string {
	if c.ArgHint == "" {
		return c.Aliases[0]
	}
	return c.Aliases[0] + " " + c.ArgHint
}

func (c Command) helpLine() string {
	line := c.Usage()
	if len(c.Aliases) > 1 {
		line += "（别名：" + strings.Join(c.Aliases[1:], "、") + "）"
	}
	if c.Desc == "" {
		return line
	}
	return line + " - " + c.Desc
}

// CommandRouter dispatches slash-prefixed commands against a static table. The
// table is immutable after construction, so a router is safe for concurrent use
// and needs no per-message rebuild.
type CommandRouter struct {
	commands []Command
}

// NewCommandRouter builds a router from the given commands. Order matters: the
// first matching command wins, so list more specific commands before any
// namespace catch-all.
func NewCommandRouter(commands ...Command) *CommandRouter {
	return &CommandRouter{commands: commands}
}

// Dispatch runs the first command matching text. reply emits the usage hint when
// a command matches but is missing required arguments. It returns true when a
// command consumed the message; false lets the caller fall back to other logic.
func (r *CommandRouter) Dispatch(ctx context.Context, msg *wechatbot.IncomingMessage, text string, reply func(string)) bool {
	text = strings.TrimSpace(text)
	if !strings.HasPrefix(text, "/") {
		return false
	}
	for _, c := range r.commands {
		args, ok := c.match(text)
		if !ok {
			continue
		}
		if len(args) < c.MinArgs {
			reply("用法：" + c.Usage())
			return true
		}
		c.Run(ctx, msg, args)
		return true
	}
	return false
}

// Help renders the (non-hidden) command listing between an optional header and
// footer.
func (r *CommandRouter) Help(header, footer string) string {
	var b strings.Builder
	if header != "" {
		b.WriteString(header)
		b.WriteByte('\n')
	}
	for _, c := range r.commands {
		if c.Hidden {
			continue
		}
		b.WriteString(c.helpLine())
		b.WriteByte('\n')
	}
	if footer != "" {
		b.WriteString(footer)
	}
	return strings.TrimRight(b.String(), "\n")
}
