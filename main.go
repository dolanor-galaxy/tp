package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	flag "github.com/cornfeedhobo/pflag"
	"github.com/gdamore/tcell/v2"
	"github.com/mattn/go-isatty"
	"github.com/rivo/tview"
	"golang.org/x/crypto/ssh/terminal"
	"golang.org/x/text/transform"
)

var (
	name    = "tp"
	version = ""
	commit  = ""
)

var (
	shell          string
	initCommand    string
	commandFlag    bool
	helpFlag       bool
	horizontalFlag bool
	versionFlag    bool
	stdinBytes     []byte
	blockCommands  = getBlockList()
)

var env = func() string {
	return os.Getenv("TP_BLOCK_COMMAND")
}

var getTerminalHeight = func() int {
	_, height, _ := terminal.GetSize(int(os.Stderr.Fd()))
	return height
}

type tui struct {
	*tview.Application
	cliPane    *cliPane
	stdinPane  *stdinViewPane
	stdoutPane *stdoutViewPane
}

func newTui() *tui {
	cliPane := newCliPane()
	stdinPane := newStdinViewPane()
	stdoutPane := newStdoutViewPane()

	flex := tview.NewFlex()
	if horizontalFlag {
		flex.SetDirection(tview.FlexRow).
			AddItem(cliPane, 1, 0, false).
			AddItem(stdinPane, 0, 1, false).
			AddItem(stdoutPane, 0, 1, false)
	} else {
		viewPanes := tview.NewFlex()
		viewPanes.SetDirection(tview.FlexColumn).
			AddItem(stdinPane, 0, 1, false).
			AddItem(stdoutPane, 0, 1, false)

		flex.SetDirection(tview.FlexRow).
			AddItem(cliPane, 1, 0, false).
			AddItem(viewPanes, 0, 1, false)
	}

	t := &tui{
		Application: tview.NewApplication(),
		cliPane:     cliPane,
		stdinPane:   stdinPane,
		stdoutPane:  stdoutPane,
	}
	t.SetRoot(flex, true).SetFocus(cliPane)
	t.setAction()
	return t
}

func (t *tui) setAction() {
	t.stdinPane.SetChangedFunc(func() {
		t.Draw()
	})

	t.stdoutPane.SetChangedFunc(func() {
		t.Draw()
	})

	t.cliPane.SetChangedFunc(func(text string) {
		_text := strings.TrimSpace(text)
		if t.cliPane.trimText == _text {
			t.cliPane.trimText = _text
			return
		}
		t.cliPane.trimText = _text
		t.stdoutPane.reset()
		t.updateStdoutView(text)
	})

	t.cliPane.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		switch event.Key() {
		case tcell.KeyCtrlF:
			return tcell.NewEventKey(tcell.KeyRight, 0, tcell.ModNone)

		case tcell.KeyCtrlB:
			return tcell.NewEventKey(tcell.KeyLeft, 0, tcell.ModNone)

		case tcell.KeyCtrlD:
			return tcell.NewEventKey(tcell.KeyDelete, 0, tcell.ModNone)

		case tcell.KeyCtrlC:
			if commandFlag {
				fmt.Println(initCommand)
				return event
			}
			return event

		case tcell.KeyEnter:
			t.stdinPane.cancel()
			t.stdoutPane.cancel()
			t.Stop()

			_text := adjustPipe(t.cliPane.prompt) + t.cliPane.GetText()
			if commandFlag {
				fmt.Println(_text)
				return nil
			}

			if isBlock(_text) {
				fmt.Fprintf(os.Stderr, "This command is blocked")
				return nil
			}

			cmd := exec.Command(shell, "-c", _text)
			cmd.Stdin = bytes.NewReader(stdinBytes)
			cmd.Stdout = os.Stdout
			cmd.Stderr = os.Stderr
			cmd.Run()
			return nil

		case tcell.KeyBackspace, tcell.KeyBackspace2:
			if t.cliPane.GetText() == "" {
				if t.cliPane.prompt == "" {
					return event
				}
				t.cliPane.setPrompt(t.cliPane.prompt)
				t.stdinPane.reset()
				t.updateStdinView()
				return nil
			}
			return event

		case tcell.KeyRune:
			switch event.Rune() {
			case '|':
				t.cliPane.addPrompt()
				t.stdinPane.reset()
				t.updateStdinView()
				return nil
			case '>':
				return nil
			case '<':
				return nil
			}
		}
		return event
	})
}

func (t *tui) start() int {
	t.updateStdinView()
	t.updateStdoutView(t.cliPane.GetText())

	if err := t.Run(); err != nil {
		t.Stop()
		return 1
	}
	return 0
}

func (t *tui) updateStdinView() {
	stdinCtx, stdinCancel := context.WithCancel(t.stdinPane.ctx)

	p := t.cliPane.prompt
	t.cliPane.syncUpdate(func() {
		t.cliPane.wg.Add(1)
	})
	go func() {
		defer t.cliPane.wg.Done()
		defer stdinCancel()
		if p == "" {
			t.stdinPane.setData(stdinBytes)
		} else {
			t.stdinPane.execCommand(stdinCtx, p, stdinBytes)
		}
	}()
	go func() {
		s := spinner()
		for {
			select {
			case <-stdinCtx.Done():
				t.QueueUpdateDraw(func() {
					t.stdoutPane.SetTitle(t.stdoutPane.name)
				})
				return
			case <-time.After(100 * time.Millisecond):
				t.QueueUpdateDraw(func() {
					t.stdoutPane.SetTitle(t.stdoutPane.name + s())
				})
			}
		}
	}()
}

func (t *tui) updateStdoutView(text string) {
	stdoutCtx, stdoutCancel := context.WithCancel(t.stdoutPane.ctx)

	go func() {
		defer stdoutCancel()
		t.cliPane.syncUpdate(func() {
			t.cliPane.wg.Wait()
		})
		t.stdinPane.syncUpdate(func() {
			t.stdoutPane.execCommand(stdoutCtx, text, t.stdinPane.data)
		})
	}()
}

func spinner() func() string {
	c := 0
	spinners := []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}
	return func() string {
		i := c % len(spinners)
		c++
		return spinners[i]
	}
}

type cliPane struct {
	*tview.InputField
	symbol   string
	prompt   string
	trimText string
	wg       sync.WaitGroup
	mu       sync.Mutex
}

func newCliPane() *cliPane {
	inputField := tview.NewInputField()
	inputField.SetAcceptanceFunc(tview.InputFieldMaxLength(200)).
		SetFieldWidth(0)

	symbol := "| "
	if bytes.Equal(stdinBytes, []byte("")) {
		symbol = "> "
	}

	c := &cliPane{
		InputField: inputField,
		symbol:     symbol,
	}
	c.setPrompt(initCommand)
	return c
}

func (c *cliPane) syncUpdate(fn func()) {
	c.mu.Lock()
	defer c.mu.Unlock()
	fn()
}

func (c *cliPane) setPrompt(text string) {
	if strings.Contains(text, "|") {
		c.prompt = text[:strings.LastIndex(text, "|")]
		c.SetLabel(c.symbol + adjustPipe(c.prompt))
		c.SetText(text[strings.LastIndex(text, "|")+1:])
		return
	}
	c.SetLabel(c.symbol)
	c.SetText(text)
	c.prompt = ""
}

func (c *cliPane) addPrompt() {
	c.prompt = adjustPipe(c.prompt) + c.GetText()
	c.SetLabel(c.symbol + adjustPipe(c.prompt)).
		SetText("")
}

func adjustPipe(text string) string {
	if text == "" {
		return ""
	}
	return text + "|"
}

type viewPane struct {
	*tview.TextView
	name   string
	ctx    context.Context
	cancel context.CancelFunc
	mu     sync.Mutex
}

func newViewPane(name string) *viewPane {
	textView := tview.NewTextView()
	textView.SetWrap(false).
		SetDynamicColors(true).
		SetScrollable(false).
		SetTitleAlign(tview.AlignLeft).
		SetTitle(name).
		SetBorder(true)

	ctx, cancel := context.WithCancel(context.Background())

	v := &viewPane{
		TextView: textView,
		name:     name,
		ctx:      ctx,
		cancel:   cancel,
	}
	return v
}

func (v *viewPane) syncUpdate(fn func()) {
	v.mu.Lock()
	defer v.mu.Unlock()
	fn()
}

func (v *viewPane) reset() {
	v.Clear()
	v.cancel()
	v.ctx, v.cancel = context.WithCancel(context.Background())
}

type stdinViewPane struct {
	*viewPane
	data []byte
}

func newStdinViewPane() *stdinViewPane {
	v := newViewPane("stdin")
	si := &stdinViewPane{
		viewPane: v,
		data:     []byte(""),
	}
	return si
}

func (si *stdinViewPane) setData(inputBytes []byte) {
	tt := newTextLineTransformer()
	w := transform.NewWriter(tview.ANSIWriter(si), tt)

	si.syncUpdate(func() {
		si.data = make([]byte, len(inputBytes))
		copy(si.data, inputBytes)
	})
	io.Copy(w, bytes.NewReader(inputBytes))
}

func (si *stdinViewPane) execCommand(ctx context.Context, text string, inputBytes []byte) {
	_data := new(bytes.Buffer)
	tt := newTextLineTransformer()
	w := transform.NewWriter(tview.ANSIWriter(si), tt)
	mw := io.MultiWriter(w, _data)

	if isBlock(text) {
		fmt.Fprint(mw, "This command is blocked")
		return
	}

	cmd := exec.CommandContext(ctx, shell, "-c", text)

	cmd.Stdin = bytes.NewReader(inputBytes)
	cmd.Stdout = mw

	cmd.Run()
	si.syncUpdate(func() {
		si.data = _data.Bytes()
	})
}

type stdoutViewPane struct {
	*viewPane
}

func newStdoutViewPane() *stdoutViewPane {
	v := newViewPane("stdout/stderr")
	so := &stdoutViewPane{
		viewPane: v,
	}
	return so
}

func (so *stdoutViewPane) execCommand(ctx context.Context, text string, inputBytes []byte) {
	tt := newTextLineTransformer()
	w := transform.NewWriter(tview.ANSIWriter(so), tt)

	if isBlock(text) {
		fmt.Fprint(w, "This command is blocked")
		return
	}

	cmd := exec.CommandContext(ctx, shell, "-c", text)

	cmd.Stdin = bytes.NewReader(inputBytes)
	cmd.Stdout = w
	cmd.Stderr = w

	cmd.Run()
}

func getBlockList() []string {
	_env := env()
	if _env == "" {
		return nil
	}
	return strings.Split(_env, ":")
}

func isBlock(text string) bool {
	if blockCommands == nil {
		return false
	}

	for _, cmd := range blockCommands {
		_text := strings.TrimLeft(text, " ")
		if _text == cmd {
			return true
		}
		if strings.HasPrefix(_text, cmd+" ") {
			return true
		}
	}
	return false
}

type textLineTransformer struct {
	transform.NopResetter
	line  int
	limit int
	temp  []byte
}

func newTextLineTransformer() *textLineTransformer {
	tt := &textLineTransformer{
		line:  0,
		limit: getTerminalHeight() - 3,
		temp:  []byte(""),
	}
	return tt
}

func (tt *textLineTransformer) Transform(dst, src []byte, atEOF bool) (nDst, nSrc int, err error) {
	if tt.limit <= tt.line {
		nSrc = len(src)
		nDst = 0
		return
	}

	nSrc = len(src)
	_src := src
	if len(tt.temp) > 0 {
		_src = make([]byte, len(tt.temp)+len(src))
		copy(_src, tt.temp)
		copy(_src[len(tt.temp):], src)
	}

	if len(_src) > 4096 {
		tt.temp = make([]byte, len(_src[4096:]))
		copy(tt.temp, _src[4096:])
		err = transform.ErrShortDst
	}

	i, b := 0, 0
	for {
		i = bytes.IndexByte(_src[b:], '\n')
		if i == -1 {
			nDst = copy(dst, _src)
			return
		}
		b = b + i + 1

		if b >= 4096 {
			nDst = copy(dst, _src)
			return
		}
		tt.line++

		if tt.limit <= tt.line {
			nDst = copy(dst, _src[:b-1])
			return
		}
	}
}

func main() {
	flag.BoolVarP(&helpFlag, "help", "h", false, "Show help")
	flag.BoolVar(&horizontalFlag, "horizontal", false, "Split the view horizontally")
	flag.BoolVarP(&versionFlag, "version", "v", false, "Show version")
	flag.BoolVarP(&commandFlag, "command", "c", false, "Return commandline text")
	flag.StringVarP(&shell, "shell", "s", os.Getenv("SHELL"), "Specify the shell to use")
	flag.Parse()

	if helpFlag {
		fmt.Fprintln(os.Stderr, "Usage of tp:")
		flag.PrintDefaults()
		os.Exit(0)
	}

	if versionFlag {
		fmt.Printf("%s version %s-%s\n", name, version, commit)
		os.Exit(0)
	}

	if os.Getenv("SHELL") == "" {
		fmt.Fprint(os.Stderr, "$SHELL not found, please specify the shell by '-s' option")
		os.Exit(1)
	}

	_, err := exec.LookPath(shell)
	if err != nil {
		fmt.Fprintf(os.Stderr, "executable file not found: '%s'", shell)
		os.Exit(1)
	}

	initCommand = flag.Arg(0)

	if !isatty.IsTerminal(os.Stdin.Fd()) {
		stdinBytes, _ = ioutil.ReadAll(os.Stdin)
	}

	t := newTui()
	os.Exit(t.start())
}
