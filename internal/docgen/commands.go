package main

import (
	"fmt"
	"strings"

	"github.com/opstruss/opstruss/internal/cli"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// commandPages are the pages of root and of every command under it that
// help shows, by file name: index.md for root, state-list.md for opstruss
// state list.
func commandPages(root *cobra.Command, short []cli.ShortFlag) map[string]string {
	pages := map[string]string{}
	var walk func(*cobra.Command)
	walk = func(c *cobra.Command) {
		pages[pageName(c)] = commandPage(c, short)
		for _, sub := range c.Commands() {
			if sub.IsAvailableCommand() {
				walk(sub)
			}
		}
	}
	walk(root)
	return pages
}

// pageName is the file of the page of c.
func pageName(c *cobra.Command) string {
	if !c.HasParent() {
		return "index.md"
	}
	return strings.ReplaceAll(strings.TrimPrefix(c.CommandPath(), c.Root().Name()+" "), " ", "-") + ".md"
}

// link is the link from a page under cli/ to the page of c.
func link(c *cobra.Command) string {
	if !c.HasParent() {
		return "./"
	}
	return "./" + strings.TrimSuffix(pageName(c), ".md")
}

// commandPage is the page of c: what it does, its usage, its flags, those
// it shares with every command, and the commands under it.
func commandPage(c *cobra.Command, short []cli.ShortFlag) string {
	var b strings.Builder
	fmt.Fprintf(&b, "---\ntitle: %s\n---\n\n%s::: v-pre\n\n# %s\n\n", c.CommandPath(), header, c.CommandPath())
	text := c.Long
	if text == "" {
		text = c.Short + "."
	}
	fmt.Fprintf(&b, "%s\n\n", prose(text))
	if c.Runnable() {
		fmt.Fprintf(&b, "## Usage\n\n```text\n%s\n```\n\n", c.UseLine())
	}
	if c.Example != "" {
		// Help indents examples by two spaces, as cobra does.
		lines := strings.Split(c.Example, "\n")
		for i, l := range lines {
			lines[i] = strings.TrimPrefix(l, "  ")
		}
		fmt.Fprintf(&b, "## Examples\n\n```sh\n%s\n```\n\n", strings.Join(lines, "\n"))
	}
	var local, global []cli.ShortFlag
	for _, f := range short {
		switch {
		case f.Global && !c.HasParent():
			local = append(local, f)
		case f.Global:
			global = append(global, f)
		case f.For(c):
			local = append(local, f)
		}
	}
	flags(&b, "Flags", local, c.LocalFlags())
	flags(&b, "Global flags", global, c.InheritedFlags())
	var subs []*cobra.Command
	for _, sub := range c.Commands() {
		if sub.IsAvailableCommand() {
			subs = append(subs, sub)
		}
	}
	if len(subs) > 0 {
		b.WriteString("## Commands\n\n| Command | Description |\n|---|---|\n")
		for _, sub := range subs {
			fmt.Fprintf(&b, "| [%s](%s) | %s |\n", code(sub.CommandPath()), link(sub), prose(sub.Short))
		}
		b.WriteString("\n")
	}
	b.WriteString(":::\n")
	return b.String()
}

// flags writes the table of the short flags and the flags of fs, under
// title, when there are any.
func flags(b *strings.Builder, title string, short []cli.ShortFlag, fs *pflag.FlagSet) {
	var rows []string
	for _, f := range short {
		name := "-" + f.Name
		if f.Arg != "" {
			name += " " + f.Arg
		}
		rows = append(rows, fmt.Sprintf("| %s | %s |", code(name), prose(strings.ReplaceAll(f.Usage, "`", ""))))
	}
	fs.VisitAll(func(f *pflag.Flag) {
		if f.Hidden || f.Name == "help" {
			return
		}
		arg, usage := pflag.UnquoteUsage(f)
		name := "--" + f.Name
		if arg != "" {
			name += " " + arg
		}
		name = code(name)
		if f.Shorthand != "" {
			name = code("-"+f.Shorthand) + ", " + name
		}
		switch f.DefValue {
		case "", "false", "[]", "0":
		default:
			usage += " (default " + f.DefValue + ")"
		}
		rows = append(rows, fmt.Sprintf("| %s | %s |", name, prose(usage)))
	})
	if len(rows) == 0 {
		return
	}
	fmt.Fprintf(b, "## %s\n\n| Flag | Description |\n|---|---|\n%s\n\n", title, strings.Join(rows, "\n"))
}
