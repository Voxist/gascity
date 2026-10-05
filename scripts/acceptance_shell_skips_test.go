package scripts_test

import (
	"fmt"
	"regexp"
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

// This file is the shell half of the acceptance -skip guard: it reads a
// workflow step's run script with a real bash parser (mvdan.cc/sh) instead
// of tracking quotes, heredocs, comments, command substitution and line
// continuations by hand. Comments never reach the AST; quoting, ANSI-C
// `$'..'`, `$( )`, backticks and continuations are the parser's problem.

// ghaExpressionPattern matches a GitHub Actions `${{ ... }}` expression. The
// shell parser cannot read these, and their value is only known at run time,
// so each one is replaced by a neutral placeholder word after its own text
// has been scanned for a skip-shaped token.
var ghaExpressionPattern = regexp.MustCompile(`\$\{\{.*?\}\}`)

const ghaExpressionPlaceholder = "GHAEXPR"

// parseRunScript parses a run script as bash. GitHub expressions are
// scanned for a skip-shaped token first (returned in exprWindow) and then
// replaced by a placeholder so the parser accepts the script.
func parseRunScript(script string) (file *syntax.File, exprWindow string, err error) {
	for _, m := range ghaExpressionPattern.FindAllString(script, -1) {
		if window, found := findSkipFlagToken(m); found && exprWindow == "" {
			exprWindow = window
		}
	}
	cleaned := ghaExpressionPattern.ReplaceAllString(script, ghaExpressionPlaceholder)
	file, err = syntax.NewParser(syntax.Variant(syntax.LangBash)).Parse(strings.NewReader(cleaned), "run")
	if err != nil {
		return nil, exprWindow, fmt.Errorf("run script does not parse as bash (%w); the -skip guard fails closed on a script it cannot read", err)
	}
	return file, exprWindow, nil
}

// wordStaticText is the concatenated static text of w: literal parts always,
// dynamic parts skipped. Used for the fail-closed scan of non-literal words
// such as `-skip=$X` or `"-skip"$E`.
func wordStaticText(w *syntax.Word) string {
	var b strings.Builder
	var add func(parts []syntax.WordPart)
	add = func(parts []syntax.WordPart) {
		for _, p := range parts {
			switch p := p.(type) {
			case *syntax.Lit:
				b.WriteString(p.Value)
			case *syntax.SglQuoted:
				b.WriteString(p.Value)
			case *syntax.DblQuoted:
				add(p.Parts)
			}
		}
	}
	add(w.Parts)
	return b.String()
}

// canonicalSkipExpr reports whether args[i] is a bare `-skip` followed by exactly
// one plain single-quoted word, returning the expression.
func canonicalSkipExpr(args []*syntax.Word, i int) (string, bool) {
	if i+1 >= len(args) {
		return "", false
	}
	// The flag itself must be a bare, unquoted `-skip`: any quoted spelling is
	// non-canonical and stays flagged.
	if len(args[i].Parts) != 1 {
		return "", false
	}
	if lit, ok := args[i].Parts[0].(*syntax.Lit); !ok || lit.Value != "-skip" {
		return "", false
	}
	next := args[i+1]
	if len(next.Parts) != 1 {
		return "", false
	}
	q, ok := next.Parts[0].(*syntax.SglQuoted)
	if !ok || q.Dollar {
		return "", false
	}
	return q.Value, true
}

// scanRunScriptSkips parses script and returns every canonical
// `-skip '<expr>'` expression, plus the window around the first other
// skip-shaped token anywhere in the script's words (flagged == "" when
// none). Deny by default: a skip-shaped word that is not exactly the
// canonical form is reported, however it is quoted or assembled.
func scanRunScriptSkips(script string) (exprs []string, flagged string, err error) {
	file, exprWindow, err := parseRunScript(script)
	if err != nil {
		return nil, "", err
	}
	flagged = exprWindow
	consumed := map[*syntax.Word]bool{}
	syntax.Walk(file, func(n syntax.Node) bool {
		if call, ok := n.(*syntax.CallExpr); ok {
			for i := 0; i < len(call.Args); i++ {
				if expr, ok := canonicalSkipExpr(call.Args, i); ok {
					exprs = append(exprs, expr)
					consumed[call.Args[i]] = true
					consumed[call.Args[i+1]] = true
					i++
				}
			}
		}
		if w, ok := n.(*syntax.Word); ok && !consumed[w] && flagged == "" {
			if window, found := findSkipFlagToken(wordStaticText(w)); found {
				flagged = window
			}
		}
		return true
	})
	return exprs, flagged, nil
}

// scriptMentionsAll reports whether every needle appears in the script's
// real (non-comment) text: literals, quoted strings and parameter names. An
// unparsable script falls back to its raw text, which can only over-trigger.
func scriptMentionsAll(script string, needles ...string) bool {
	file, _, err := parseRunScript(script)
	text := script
	if err == nil {
		var b strings.Builder
		syntax.Walk(file, func(n syntax.Node) bool {
			switch n := n.(type) {
			case *syntax.Lit:
				b.WriteString(n.Value)
				b.WriteByte(' ')
			case *syntax.ParamExp:
				if n.Param != nil {
					b.WriteString(n.Param.Value)
					b.WriteByte(' ')
				}
			}
			return true
		})
		text = b.String()
	}
	for _, needle := range needles {
		if !strings.Contains(text, needle) {
			return false
		}
	}
	return true
}
