package edit

import (
	"errors"
	"os"
	"path"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/elves/elvish/edit/lscolors"
	"github.com/elves/elvish/edit/tty"
	"github.com/elves/elvish/edit/ui"
	"github.com/elves/elvish/eval"
	"github.com/elves/elvish/parse"
	"github.com/elves/elvish/util"
)

// Navigation subsystem.

// Interface.

var _ = registerBuiltins(modeNavigation, map[string]func(*Editor){
	"start":                    navStart,
	"up":                       navUp,
	"down":                     navDown,
	"page-up":                  navPageUp,
	"page-down":                navPageDown,
	"left":                     navLeft,
	"right":                    navRight,
	"trigger-shown-hidden":     navTriggerShowHidden,
	"trigger-filter":           navTriggerFilter,
	"insert-selected":          navInsertSelected,
	"insert-selected-and-quit": navInsertSelectedAndQuit,
	"enter-cwd":                navEnterCWD,
	"quit":                     quit,
	"default":                  navDefault,
})

var Dir = ""

func init() {
	registerBindings(modeNavigation, modeNavigation, map[ui.Key]string{
		{ui.Up, 0}:       "up",
		{ui.Down, 0}:     "down",
		{ui.PageUp, 0}:   "page-up",
		{ui.PageDown, 0}: "page-down",
		{'v', ui.Alt}:    "page-up",
		{'V', ui.Ctrl}:   "page-down",
		{'P', ui.Ctrl}:   "left",
		{'H', ui.Ctrl}:   "left",
		{ui.Left, 0}:     "left",
		{'L', ui.Ctrl}:   "right",
		{ui.Right , 0}:   "right",
		{'[', ui.Ctrl}:   "quit",
		{ui.Enter, 0}:    "insert-selected-and-quit",
		{'E', ui.Ctrl}:   "insert-selected-and-quit",
		{'D', ui.Ctrl}:   "enter-cwd",
		{'F', ui.Ctrl}:   "trigger-shown-hidden",
		ui.Default:       "default",
	})
}

type navigation struct {
	current    *navColumn
	parent     *navColumn
	preview    *navColumn
	showHidden bool
	filtering  bool
	filter     string
	chdir      func(string) error
}

func (*navigation) Binding(m map[string]eval.Variable, k ui.Key) eval.CallableValue {
	return getBinding(m[modeNavigation], k)
}

func (n *navigation) ModeLine() ui.Renderer {
	title := " NAVIGATING "
	if n.showHidden {
		title += "(show hidden) "
	}
	return modeLineRenderer{title, n.filter}
}

func (n *navigation) CursorOnModeLine() bool {
	return n.filtering
}

func navStart(ed *Editor) {
	initNavigation(&ed.navigation, ed)
	ed.mode = &ed.navigation
	navTriggerFilter(ed)
	// navTriggerShowHidden(ed)
}

func navUp(ed *Editor) {
	ed.navigation.prev()
}

func navDown(ed *Editor) {
	ed.navigation.next()
}

func navPageUp(ed *Editor) {
	ed.navigation.current.pageUp()
	ed.navigation.refresh()
}

func navPageDown(ed *Editor) {
	ed.navigation.current.pageDown()
	ed.navigation.refresh()
}

func navLeft(ed *Editor) {
	ed.navigation.ascend()
}

func navRight(ed *Editor) {
	ed.navigation.descend()
}

func navTriggerShowHidden(ed *Editor) {
	ed.navigation.showHidden = !ed.navigation.showHidden
	ed.navigation.refresh()
}

func navTriggerFilter(ed *Editor) {
	ed.navigation.filtering = !ed.navigation.filtering
}

func navInsertSelected(ed *Editor) {
	ed.insertAtDot(parse.Quote(ed.navigation.current.selectedName()) + " ")
}

func quit(ed *Editor) {
	go func() {
		ed.reader.UnitChan() <- tty.Key{'G', ui.Ctrl}
	}()
}

func navEnterCWD(ed *Editor) {
	ed.mode = &ed.insert
	wd, _ := os.Getwd()
	Dir = wd
	go func() {
		ed.reader.UnitChan() <- tty.Key{'G', ui.Ctrl}
	}()
}

func navInsertSelectedAndQuit(ed *Editor) {
	ed.mode = &ed.insert
	wd, _ := os.Getwd()
	Dir = wd + "/" + parse.Quote(ed.navigation.current.selectedName())
	go func() {
		ed.reader.UnitChan() <- tty.Key{'G', ui.Ctrl}
	}()
}

func navDefault(ed *Editor) {
	// Use key binding for insert mode without exiting nigation mode.
	k := ed.lastKey
	n := &ed.navigation
	if n.filtering && likeChar(k) {
		n.filter += k.String()
		n.refreshCurrent()
		n.refreshDirPreview()
	} else if n.filtering && k == (ui.Key{ui.Backspace, 0}) {
		_, size := utf8.DecodeLastRuneInString(n.filter)
		if size > 0 {
			n.filter = n.filter[:len(n.filter)-size]
			n.refreshCurrent()
			n.refreshDirPreview()
		}
	} else if n.filtering && k == (ui.Key{'U', ui.Ctrl}) {
		n.filter = ""
		n.refreshCurrent()
		n.refreshDirPreview()
	} else {
		ed.CallFn(getBinding(ed.bindings[modeInsert], k))
	}
}

// Implementation.
// TODO(xiaq): Remember which file was selected in each directory.

var (
	errorEmptyCwd      = errors.New("current directory is empty")
	errorNoCwdInParent = errors.New("could not find current directory in parent")
)

func initNavigation(n *navigation, ed *Editor) {
	*n = navigation{chdir: ed.chdir}
	n.refresh()
}

func (n *navigation) maintainSelected(name string) {
	n.current.selected = 0
	for i, s := range n.current.candidates {
		if s.Text > name {
			break
		}
		n.current.selected = i
	}
}

func (n *navigation) refreshCurrent() {
	selectedName := n.current.selectedName()
	all, err := n.loaddir(".")
	if err != nil {
		n.current = newErrNavColumn(err)
		return
	}
	// Try to select the old selected file.
	// XXX(xiaq): This would break when we support alternative ordering.
	n.current = newNavColumn(all, func(i int) bool {
		return i == 0 || all[i].Text <= selectedName
	})
	n.current.changeFilter(n.filter)
	n.maintainSelected(selectedName)
}

func (n *navigation) refreshParent() {
	wd, err := os.Getwd()
	if err != nil {
		n.parent = newErrNavColumn(err)
		return
	}
	if wd == "/" {
		n.parent = newNavColumn(nil, nil)
	} else {
		all, err := n.loaddir("..")
		if err != nil {
			n.parent = newErrNavColumn(err)
			return
		}
		cwd, err := os.Stat(".")
		if err != nil {
			n.parent = newErrNavColumn(err)
			return
		}
		n.parent = newNavColumn(all, func(i int) bool {
			d, _ := os.Lstat("../" + all[i].Text)
			return os.SameFile(d, cwd)
		})
	}
}

func (n *navigation) refreshDirPreview() {
	if n.current.selected != -1 {
		name := n.current.selectedName()
		fi, err := os.Stat(name)
		if err != nil {
			n.preview = newErrNavColumn(err)
			return
		}
		if fi.Mode().IsDir() {
			all, err := n.loaddir(name)
			if err != nil {
				n.preview = newErrNavColumn(err)
				return
			}
			n.preview = newNavColumn(all, func(int) bool { return false })
		} else {
			n.preview = newFilePreviewNavColumn(name)
		}
	} else {
		n.preview = nil
	}
}

// refresh rereads files in current and parent directories and maintains the
// selected file if possible.
func (n *navigation) refresh() {
	n.refreshCurrent()
	n.refreshParent()
	n.refreshDirPreview()
}

// ascend changes current directory to the parent.
// TODO(xiaq): navigation.{ascend descend} bypasses the cd builtin. This can be
// problematic if cd acquires more functionality (e.g. trigger a hook).
func (n *navigation) ascend() error {
	wd, err := os.Getwd()
	if err != nil {
		return err
	}
	if wd == "/" {
		return nil
	}

	name := n.parent.selectedName()
	err = os.Chdir("..")
	if err != nil {
		return err
	}
	n.filter = ""
	n.refresh()
	n.maintainSelected(name)
	// XXX Refresh dir preview again. We should perhaps not have used refresh
	// above.
	n.refreshDirPreview()
	return nil
}

// descend changes current directory to the selected file, if it is a
// directory.
func (n *navigation) descend() error {
	if n.current.selected == -1 {
		return errorEmptyCwd
	}
	name := n.current.selectedName()
	err := n.chdir(name)
	if err != nil {
		return err
	}
	n.filter = ""
	n.current.selected = -1
	n.refresh()
	n.refreshDirPreview()
	return nil
}

// prev selects the previous file.
func (n *navigation) prev() {
	if n.current.selected > 0 {
		n.current.selected--
	}
	n.refresh()
}

// next selects the next file.
func (n *navigation) next() {
	if n.current.selected != -1 && n.current.selected < len(n.current.candidates)-1 {
		n.current.selected++
	}
	n.refresh()
}

func (n *navigation) loaddir(dir string) ([]ui.Styled, error) {
	f, err := os.Open(dir)
	if err != nil {
		return nil, err
	}
	infos, err := f.Readdir(-1)
	var names []string
	for _, info := range infos {
		if info.IsDir() {
			names = append(names, info.Name())
		}
	}
	sort.Strings(names)
	var all []ui.Styled
	lsColor := lscolors.GetColorist()
	for _, name := range names {
		if n.showHidden || name[0] != '.' {
			all = append(all, ui.Styled{name,
				ui.StylesFromString(lsColor.GetStyle(path.Join(dir, name)))})
		}
	}

	return all, nil
}

const (
	navigationListingColMargin = 1

	parentColumnWeight  = 3.0
	currentColumnWeight = 8.0
	previewColumnWeight = 9.0
)

func (n *navigation) List(maxHeight int) ui.Renderer {
	return makeNavRenderer(
		maxHeight,
		n.parent.FullWidth(maxHeight),
		n.current.FullWidth(maxHeight),
		n.preview.FullWidth(maxHeight),
		n.parent.List(maxHeight),
		n.current.List(maxHeight),
		n.preview.List(maxHeight),
	)
}

// navColumn is a column in the navigation layout.
type navColumn struct {
	listing
	all        []ui.Styled
	candidates []ui.Styled
	// selected int
	err error
}

func newNavColumn(all []ui.Styled, sel func(int) bool) *navColumn {
	nc := &navColumn{all: all, candidates: all}
	nc.provider = nc
	nc.selected = -1
	for i := range all {
		if sel(i) {
			nc.selected = i
		}
	}
	return nc
}

func newErrNavColumn(err error) *navColumn {
	nc := &navColumn{err: err}
	nc.provider = nc
	return nc
}

// PreviewBytes is the maximum number of bytes to preview a file.
const PreviewBytes = 64 * 1024

// Errors displayed in the preview area.
var (
	ErrNotRegular   = errors.New("no preview for non-regular file")
	ErrNotValidUTF8 = errors.New("no preview for non-utf8 file")
)

func newFilePreviewNavColumn(fname string) *navColumn {
	// XXX This implementation is a bit hacky, since listing is not really
	// intended for listing file content.
	var err error
	file, err := os.Open(fname)
	if err != nil {
		return newErrNavColumn(err)
	}

	info, err := file.Stat()
	if err != nil {
		return newErrNavColumn(err)
	}
	if (info.Mode() & (os.ModeDevice | os.ModeNamedPipe | os.ModeSocket | os.ModeCharDevice)) != 0 {
		return newErrNavColumn(ErrNotRegular)
	}

	// BUG when the file is bigger than the buffer, the scrollbar is wrong.
	buf := make([]byte, PreviewBytes)
	nr, err := file.Read(buf[:])
	if err != nil {
		return newErrNavColumn(err)
	}

	content := string(buf[:nr])
	if !utf8.ValidString(content) {
		return newErrNavColumn(ErrNotValidUTF8)
	}

	lines := strings.Split(content, "\n")
	styleds := make([]ui.Styled, len(lines))
	for i, line := range lines {
		styleds[i] = ui.Styled{strings.Replace(line, "\t", "    ", -1), ui.Styles{}}
	}
	return newNavColumn(styleds, func(int) bool { return false })
}

func (nc *navColumn) Placeholder() string {
	if nc.err != nil {
		return nc.err.Error()
	}
	return ""
}

func (nc *navColumn) Len() int {
	return len(nc.candidates)
}

func (nc *navColumn) Show(i int) (string, ui.Styled) {
	cand := nc.candidates[i]
	return "", ui.Styled{" " + cand.Text + " ", cand.Styles}
}

func subseqstr(a string, b string) bool {
	if len(b) == 0 {
		return true
	}
	lower_a := strings.ToLower(a)
	lower_b := []rune(strings.ToLower(b))
	lb := len(lower_b)
	i := 0
	for _, c := range lower_a {
		if c == lower_b[i] {
			i++
		}
		if i == lb {
			return true
		}
	}
	return false
}

func (nc *navColumn) Filter(filter string) int {
	nc.candidates = nc.candidates[:0]
	for _, s := range nc.all {
		if subseqstr(s.Text, filter) {
			nc.candidates = append(nc.candidates, s)
		}
	}
	return 0
}

func (nc *navColumn) FullWidth(h int) int {
	if nc == nil {
		return 0
	}
	if nc.err != nil {
		return util.Wcswidth(nc.err.Error())
	}
	maxw := 0
	for _, s := range nc.candidates {
		maxw = max(maxw, util.Wcswidth(s.Text)+2)
	}
	if len(nc.candidates) > h {
		maxw++
	}
	return maxw
}

func (nc *navColumn) Accept(i int, ed *Editor) {
	// TODO
}

func (nc *navColumn) ModeTitle(i int) string {
	// Not used
	return ""
}

func (nc *navColumn) selectedName() string {
	if nc == nil || nc.selected == -1 || nc.selected >= len(nc.candidates) {
		return ""
	}
	return nc.candidates[nc.selected].Text
}
