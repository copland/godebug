package godebug

import (
	"bufio"
	"fmt"
	"os"
	"reflect"
	"strings"
	"sync/atomic"
	"unicode"

	"github.com/jtolds/gls"
)

// Scope represents a lexical scope for variable bindings.
type Scope struct {
	vars, consts map[string]interface{}
	parent       *Scope
	fileText     []string
}

// EnteringNewScope returns a new Scope and internally sets
// the current scope to be the returned scope.
func EnteringNewScope(fileText string) *Scope {
	return &Scope{
		vars:     make(map[string]interface{}),
		consts:   make(map[string]interface{}),
		fileText: parseLines(fileText),
	}
}

func parseLines(text string) []string {
	lines := strings.Split(text, "\n")

	// Trailing newline is not a separate line
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}

	return lines
}

// EnteringNewChildScope returns a new Scope that is the
// child of s and internally sets the current scope to be
// the returned scope.
func (s *Scope) EnteringNewChildScope() *Scope {
	return &Scope{
		vars:     make(map[string]interface{}),
		consts:   make(map[string]interface{}),
		parent:   s,
		fileText: s.fileText,
	}
}

func (s *Scope) getIdent(name string) (i interface{}, ok bool) {
	// TODO: This can race with other goroutines setting the value you are printing.
	for scope := s; scope != nil; scope = scope.parent {
		if i, ok = scope.vars[name]; ok {
			return dereference(i), true
		}
		if i, ok = scope.consts[name]; ok {
			return i, true
		}
	}
	return nil, false
}

// Declare creates new variable bindings in s from a list of name, value pairs.
// The values should be pointers to the values in the program rather than copies
// of them so that s can track changes to them.
func (s *Scope) Declare(namevalue ...interface{}) {
	s.addIdents(s.vars, "Declare", namevalue...)
}

// Constant is like Declare, but for constants. The values must be passed directly.
func (s *Scope) Constant(namevalue ...interface{}) {
	s.addIdents(s.consts, "Constant", namevalue...)
}

func (s *Scope) addIdents(to map[string]interface{}, funcName string, namevalue ...interface{}) {
	var i int
	for i = 0; i+1 < len(namevalue); i += 2 {
		name, ok := namevalue[i].(string)
		if !ok {
			panic(fmt.Sprintf("programming error: got odd-numbered argument to %s that was not a string", funcName))
		}
		to[name] = namevalue[i+1]
	}
	if i != len(namevalue) {
		panic(fmt.Sprintf("programming error: called %s with odd number of arguments", funcName))
	}
}

const (
	run int32 = iota
	next
	step
)

var (
	currentState     int32
	currentDepth     int
	debuggerDepth    int
	justLeft         bool // we returned from a function we were stepping through and have not yet run any debug code in the parent function
	context          = getPreferredContextManager()
	goroutineKey     = gls.GenSym()
	currentGoroutine uint32
	ids              idPool
)

// EnterFunc marks the beginning of a function. Calling fn should be equivalent to running
// the function that is being entered. If proceed is false, EnterFunc did in fact call
// fn, and so the caller of EnterFunc should return immediately rather than proceed to
// duplicate the effects of fn.
func EnterFunc(fn func()) (ctx *Context, proceed bool) {
	// We've entered a new function. If we're in step or next mode we have some bookkeeping to do,
	// but only if the current goroutine is the one the debugger is following.
	//
	// We consult context to determine whether we are the goroutine the debugger is following. If
	// context has not seen our goroutine before, the ok it returns is false. Why would that happen?
	// godebug supports generating debug code for a library that is later built into a binary. If that
	// happens, then context will not see any goroutines until they call code from the debugged library.
	// context will also not see any goroutines while the debugger is in state run.
	val, ok := context.GetValue(goroutineKey)
	if !ok {
		// This is the first time context has seen the current goroutine.
		//
		// Or, more accurately and precisely: This is the first frame in the current stack that contains
		// code that has been generated by godebug.
		//
		// We record some bookkeeping information with context and then continue running. This means we will
		// invoke fn, which means the caller should not proceed. After running it, return false.
		id := uint32(ids.Acquire())
		defer ids.Release(uint(id))
		context.SetValues(gls.Values{goroutineKey: id}, fn)
		return nil, false
	}
	if val.(uint32) == atomic.LoadUint32(&currentGoroutine) && currentState != run {
		if justLeft {
			// This means this goroutine ran ExitFunc followed by EnterFunc with no intervening debug calls,
			// probably because the parent caller is in another package which has not been instrumented.
			debuggerDepth++
			justLeft = false
		}
		currentDepth++
	}
	return &Context{goroutine: val.(uint32)}, true
}

// EnterFuncLit is like EnterFunc, but intended for function literals. The passed callback takes a *Context rather than no input.
func EnterFuncLit(fn func(*Context)) (ctx *Context, proceed bool) {
	val, ok := context.GetValue(goroutineKey)
	if !ok {
		id := uint32(ids.Acquire())
		defer ids.Release(uint(id))
		context.SetValues(gls.Values{goroutineKey: id}, func() {
			fn(&Context{goroutine: id})
		})
		return nil, false
	}
	if val.(uint32) == atomic.LoadUint32(&currentGoroutine) && currentState != run {
		if justLeft {
			// This means this goroutine ran ExitFunc followed by EnterFuncLit with no intervening debug calls,
			// probably because the parent caller is in another package which has not been instrumented.
			debuggerDepth++
			justLeft = false
		}
		currentDepth++
	}
	return &Context{goroutine: val.(uint32)}, true
}

// ExitFunc marks the end of a function.
func ExitFunc(ctx *Context) {
	if atomic.LoadUint32(&currentGoroutine) != ctx.goroutine {
		return
	}
	if currentState == run {
		return
	}
	if currentState == next && currentDepth == debuggerDepth {
		debuggerDepth--
		justLeft = true
	}
	currentDepth--
}

// Context contains debugging context information.
type Context struct {
	goroutine uint32
}

type caseSentinel int

// Case marks a case clause. Intended to be inserted as its own case clause
// immediately prior to the case clause it is marking.
func Case(c *Context, s *Scope, line int) interface{} {
	Line(c, s, line)
	return caseSentinel(0)
}

// Comm marks a case in a select statement.
// It returns a nil channel to read from as a new case immediately before the case it is marking.
func Comm(c *Context, s *Scope, line int) chan struct{} {
	Line(c, s, line)
	return nil
}

// EndSelect marks the end of a select statement.
// It returns a nil channel to read from as the last case of that select statement.
func EndSelect(c *Context, s *Scope) chan struct{} {
	if shouldPause(c) {
		fmt.Println("< All channel expressions evaluated. Choosing case to proceed. >")
	}
	return nil
}

// Select marks a select statement.
func Select(c *Context, s *Scope, line int) {
	if !shouldPause(c) {
		return
	}
	Line(c, s, line)
	// Assumes the debugger hasn't switched goroutines. Valid assumption now,
	// will probably change in the future.
	if currentState != run {
		fmt.Println("< Evaluating channel expressions and RHS of send expressions. >")
	}
}

// Line marks a normal line where the debugger might pause.
func Line(c *Context, s *Scope, line int) {
	lineWithPrefix(c, s, line, "")
}

func shouldPause(c *Context) bool {
	return atomic.LoadUint32(&currentGoroutine) == c.goroutine &&
		(currentState == step || (currentState == next && currentDepth == debuggerDepth))
}

func lineWithPrefix(c *Context, s *Scope, line int, prefix string) {
	if !shouldPause(c) {
		return
	}
	debuggerDepth = currentDepth
	justLeft = false
	fmt.Print("-> ", prefix, strings.TrimSpace(s.fileText[line-1]), "\n") // token.Position.Line starts at 1.
	waitForInput(s, line)
}

var skipNextElseIfExpr bool

// ElseIfSimpleStmt marks a simple statement preceding an "else if" expression.
func ElseIfSimpleStmt(c *Context, s *Scope, line int) {
	Line(c, s, line)
	if currentState == next {
		skipNextElseIfExpr = true
	}
}

// ElseIfExpr marks an "else if" expression.
func ElseIfExpr(c *Context, s *Scope, line int) {
	if atomic.LoadUint32(&currentGoroutine) != c.goroutine {
		return
	}
	if skipNextElseIfExpr {
		skipNextElseIfExpr = false
		return
	}
	Line(c, s, line)
}

// Defer marks a defer statement. Intended to be run in a defer statement of its own
// after the corresponding defer in the original source.
func Defer(c *Context, s *Scope, line int) {
	lineWithPrefix(c, s, line, "<Running deferred function>: ")
}

// SetTrace is the entrypoint to the debugger. The code generator converts
// this call to a call to SetTraceGen.
func SetTrace() {
}

// SetTraceGen is the generated entrypoint to the debugger.
func SetTraceGen(ctx *Context) {
	// TODO: The case where the user calls SetTrace multiple times has not been thought out at all yet.
	if atomic.LoadInt32(&currentState) != run {
		return
	}
	atomic.StoreUint32(&currentGoroutine, ctx.goroutine)
	currentState = step
}

var input *bufio.Scanner

func init() {
	input = bufio.NewScanner(os.Stdin)
}

var help = `
Commands:
    (h) help: Print this help.
    (n) next: Run the next line.
    (s) step: Run for one step.
    (c) continue: Run until the next breakpoint.
    (l) list: Show the current line in context of the code around it.
    (p) print <var>: Print a variable.

Commands may be given by their full name or by their parenthesized abbreviation.
Any input that is not one of the above commands is interpreted as a variable name.
`

func waitForInput(scope *Scope, line int) {
	for {
		fmt.Print("(godebug) ")
		if !input.Scan() {
			fmt.Println("quitting session")
			currentState = run
			return
		}
		s := input.Text()
		switch s {
		case "?", "h", "help":
			fmt.Println(help)
			continue
		case "n", "next":
			currentState = next
			return
		case "s", "step":
			currentState = step
			return
		case "c", "continue":
			currentState = run
			return
		case "l", "list":
			printContext(scope.fileText, line, 4)
			continue
		}
		if v, ok := scope.getIdent(strings.TrimSpace(s)); ok {
			fmt.Printf("%#v\n", v)
			continue
		}
		var cmd, name string
		n, _ := fmt.Sscan(s, &cmd, &name)
		if n == 2 && (cmd == "p" || cmd == "print") {
			if v, ok := scope.getIdent(strings.TrimSpace(name)); ok {
				fmt.Printf("%#v\n", v)
				continue
			}
		}
		fmt.Printf("Command not recognized, sorry! You typed: %q\n", s)
	}
}

func dereference(i interface{}) interface{} {
	return reflect.ValueOf(i).Elem().Interface()
}

func printContext(lines []string, line, contextCount int) {
	line-- // token.Position.Line starts at 1.
	fmt.Println()
	for i := line - contextCount; i <= line+contextCount; i++ {
		prefix := "    "
		if i == line {
			prefix = "--> "
		}
		if i >= 0 && i < len(lines) {
			line := strings.TrimRightFunc(prefix+lines[i], unicode.IsSpace)
			fmt.Println(line)
		}
	}
	fmt.Println()
}
