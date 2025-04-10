package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/gdamore/tcell/v2"
	fzf "github.com/junegunn/fzf/src"
	"github.com/rivo/tview"
	"golang.design/x/clipboard"
	"gopkg.in/yaml.v3"
)

const (
	sep = " > "

	pageMain  = "main"
	pageError = "error"
	buttonOK  = "OK"
	pageEnvs  = "envs"
)

var dots = []string{"⣾ ", "⣽ ", "⣻ ", "⢿ ", "⡿ ", "⣟ ", "⣯ ", "⣷ "}

type Config struct {
	Root Item `yaml:"root"`
}

type App struct {
	app         *tview.Application
	pages       *tview.Pages
	tree        *tview.TreeView
	textView    *tview.TextView
	search      *tview.InputField
	error       *tview.Modal
	pathToNodes map[string]*tview.TreeNode
	pathToItems map[string]Item
}

type Item struct {
	Name      string `yaml:"name,omitempty"`
	OnExpand  string `yaml:"onExpand,omitempty"`
	OnPreview string `yaml:"onPreview,omitempty"`
	OnSelect  string `yaml:"onSelect"`
	Stream    bool   `yaml:"stream"`
	Envs      []Env  `yaml:"envs"`
	Children  []Item `yaml:"children,omitempty"`
}

type Env struct {
	Name  string `yaml:"name"`
	Width int    `yaml:"width"`
}

type Reference struct {
	item   Item
	parent *tview.TreeNode
	path   string
}

func main() {
	cFlag := flag.String("c", "config.yaml", "Path to the config file")
	flag.Parse()

	a := newApp()
	if err := a.run(cFlag); err != nil {
		log.Fatal(err)
	}
}

func newApp() *App {
	modal := tview.NewModal()
	modal.AddButtons([]string{buttonOK})

	tree := tview.NewTreeView()
	tree.SetBorder(true)

	textView := tview.NewTextView().
		SetDynamicColors(true).
		SetRegions(true).
		SetWordWrap(true)
	textView.SetBorder(true)

	search := tview.NewInputField()
	search.SetTitle("Search").SetBorder(true)

	return &App{
		app:         tview.NewApplication(),
		pages:       tview.NewPages(),
		tree:        tree,
		textView:    textView,
		search:      search,
		error:       modal,
		pathToNodes: make(map[string]*tview.TreeNode),
		pathToItems: make(map[string]Item),
	}
}

func (a *App) run(cFlag *string) error {
	f, err := os.ReadFile(*cFlag)
	if err != nil {
		return fmt.Errorf("open file: %s", err)
	}

	var c *Config
	if err := yaml.Unmarshal(f, &c); err != nil {
		return fmt.Errorf("unmarshal: %w", err)
	}

	rootName := c.Root.Name
	root := tview.NewTreeNode(rootName).
		SetColor(tcell.ColorRed)
	tree := a.tree.SetRoot(root).SetCurrentNode(root)

	add := func(node *tview.TreeNode, path string, parentName *string, items []Item) error {
		node.SetColor(tcell.ColorRed)

		for _, item := range items {
			if item.OnExpand != "" {
				a.addChildItems(node, path, parentName, item.OnExpand, item.Children)
			}

			if item.Name != "" {
				a.addChildNode(node, path, item.Name, item.Children)
			}
		}

		return nil
	}

	if err := add(root, rootName, nil, c.Root.Children); err != nil {
		return fmt.Errorf("add child: %w", err)
	}

	var (
		currentCmd       *exec.Cmd
		cancelCurrentCmd chan struct{}
	)
	tree.SetSelectedFunc(func(node *tview.TreeNode) {
		reference := node.GetReference()
		if reference == nil {
			return
		}
		ref, ok := reference.(*Reference)
		if !ok {
			a.showError(errors.New("This node has wrong reference type"))
			return
		}
		children := node.GetChildren()
		if len(children) == 0 {
			parentName := ref.parent.GetText()
			if isLeafNode(ref.item.Children) {
				w := tview.ANSIWriter(a.textView)
				command := ref.item.Children[0].OnSelect
				if ref.item.Children[0].Stream {
					command = os.Expand(command, func(variable string) string {
						switch variable {
						case "current":
							return fmt.Sprintf("%q", ref.item.Name)
						case "parent":
							return parentName
						}

						return fmt.Sprintf("$%s", variable)
					})

					if currentCmd != nil && cancelCurrentCmd != nil {
						close(cancelCurrentCmd)
						if err := currentCmd.Process.Kill(); err != nil {
							a.showError(err)
						}
					}

					cancelCurrentCmd = make(chan struct{})

					expandedCommand := command
					if strings.Contains(command, "|") {
						expandedCommand = fmt.Sprintf("set -o pipefail; %s", command)
					}
					cmd := exec.Command("sh", "-c", expandedCommand)
					currentCmd = cmd
					stdout, err := cmd.StdoutPipe()
					if err != nil {
						a.showError(err)
						return
					}

					if err := cmd.Start(); err != nil {
						a.showError(err)
						return
					}

					stdoutReader := bufio.NewScanner(stdout)
					go func(cancel chan struct{}) {
						for {
							select {
							case <-cancel:
								return
							default:
								if stdoutReader.Scan() {
									if json.Valid(stdoutReader.Bytes()) {
										var data map[string]any
										if err := json.Unmarshal(stdoutReader.Bytes(), &data); err != nil {
											a.showError(err)
											return
										}

										prettyJSON, err := json.MarshalIndent(data, "", "  ")
										if err != nil {
											a.showError(err)
											return
										}

										fmt.Fprintln(w, string(prettyJSON))
									} else {
										fmt.Fprintln(w, stdoutReader.Text())
									}
								}
							}
						}
					}(cancelCurrentCmd)
				} else {
					envs := ref.item.Children[0].Envs
					if len(envs) != 0 {
						a.addEnvs(envs, ref.item.Children[0].OnSelect, []*tview.TreeNode{node}, &parentName)
					} else {
						a.execCommand([]*tview.TreeNode{node}, &parentName, ref.item.Children[0].OnSelect, true)
					}
				}
			} else {
				add(node, ref.path, &parentName, ref.item.Children)
			}
		} else {
			// Collapse if visible, expand if collapsed.
			node.SetExpanded(!node.IsExpanded())
		}
	})

	a.tree.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		switch event.Rune() {
		case 'h':
			reference := tree.GetCurrentNode().GetReference()
			if reference == nil {
				return nil
			}

			ref, ok := reference.(*Reference)
			if !ok {
				return nil
			}

			if ref.parent != tree.GetRoot() {
				tree.SetCurrentNode(ref.parent)
				tree.GetCurrentNode().SetExpanded(false)
			}
		case 'l':
			if selectedFunc := tree.GetSelectedFunc(); selectedFunc != nil {
				selectedFunc(tree.GetCurrentNode())
			}
		case 'p':
			node := tree.GetCurrentNode()
			reference := node.GetReference()
			if reference == nil {
				return nil
			}
			ref, ok := reference.(*Reference)
			if !ok {
				return nil
			}
			children := node.GetChildren()
			if len(children) == 0 {
				parentName := ref.parent.GetText()
				if isLeafNode(ref.item.Children) {
					a.execCommand([]*tview.TreeNode{node}, &parentName, ref.item.Children[0].OnPreview, false)
				}
			}
		case 'r':
			reference := tree.GetCurrentNode().GetReference()
			if reference == nil {
				return nil
			}

			ref, ok := reference.(*Reference)
			if !ok {
				return nil
			}

			node := tree.GetCurrentNode()
			node.ClearChildren()

			parentName := ref.parent.GetText()
			add(node, ref.path, &parentName, ref.item.Children)
		case '/':
			a.app.Suspend(func() {
				if err := a.fzf(); err != nil {
					return
				}
			})
		}

		return event
	})

	a.textView.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		switch event.Rune() {
		case 'h':
			a.app.SetFocus(a.tree)
		case '/':
			a.app.SetFocus(a.search)
		case 'y':
			if err := clipboard.Init(); err != nil {
				a.showError(err)
				return nil
			}
			clipboard.Write(clipboard.FmtText, []byte(a.textView.GetText(true)))
		}
		return event
	})

	a.textView.SetChangedFunc(func() {
		a.app.Draw()
		a.textView.ScrollToEnd()
	})

	a.app.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		switch event.Key() {
		case tcell.KeyTab:
			a.app.SetFocus(tree)
		case tcell.KeyCtrlL:
			a.app.SetFocus(a.textView)
		case tcell.KeyEsc:
			a.pages.SwitchToPage(pageMain)
		}
		return event
	})

	a.search.SetDoneFunc(func(key tcell.Key) {
		searchText := a.search.GetText()
		switch key {
		case tcell.KeyEnter:
			n := 0
			for _, word := range strings.Split(a.textView.GetText(true), " ") {
				if word == searchText {
					word = fmt.Sprintf(`["%d"]%s[""]`, n, searchText)
					n++
				}
				fmt.Fprintf(a.textView, "%s ", word)
			}

			currentSelection := a.textView.GetHighlights()
			if len(currentSelection) > 0 {
				a.textView.Highlight()
			} else {
				a.textView.Highlight("0").ScrollToHighlight()
			}

			a.app.SetFocus(a.textView)
		}
	})

	mainFlex := tview.NewFlex().SetDirection(tview.FlexColumn).
		AddItem(tree, 0, 3, true).
		AddItem(tview.NewFlex().SetDirection(tview.FlexRow).
			AddItem(a.textView, 0, 1, false).
			AddItem(a.search, 3, 0, false), 0, 7, false)

	a.pages.AddPage(pageMain, mainFlex, true, true)
	if err := a.app.SetRoot(a.pages, true).SetFocus(tree).Run(); err != nil {
		return fmt.Errorf("run: %w", err)
	}

	return nil
}

func (a *App) addChildNode(node *tview.TreeNode, path, text string, children []Item) {
	item := Item{
		Name:     text,
		Children: children,
	}
	ref := &Reference{
		item:   item,
		parent: node,
		path:   path + sep + text,
	}
	childNode := tview.NewTreeNode(text).
		SetText(text).
		SetReference(ref).
		SetSelectable(true)
	node.AddChild(childNode)
	a.pathToNodes[ref.path] = childNode
	a.pathToItems[ref.path] = item.Children[0]
}

func isLeafNode(items []Item) bool {
	return len(items) == 1 && items[0].OnSelect != ""
}

func (a *App) fzf() error {
	var paths []string
	for path := range a.pathToNodes {
		paths = append(paths, path)
	}

	inputChan := make(chan string)
	go func() {
		for _, s := range paths {
			inputChan <- s
		}
		close(inputChan)
	}()

	outputChan := make(chan string)
	var (
		nodes      []*tview.TreeNode
		parentName string
		item       Item
		onSelect   string
	)
	go func() {
		for path := range outputChan {
			node, ok := a.pathToNodes[path]
			if ok {
				nodes = append(nodes, node)

				if parentName == "" {
					reference := node.GetReference()
					if reference == nil {
						return
					}
					ref, ok := reference.(*Reference)
					if !ok {
						a.showError(errors.New("This node has wrong reference type"))
						return
					}
					parentName = ref.parent.GetText()
				}
			}

			item, ok = a.pathToItems[path]
			if ok && onSelect == "" {
				onSelect = item.OnSelect
			}
		}
	}()

	options, err := fzf.ParseOptions(
		true,
		[]string{"--height=50%", "--multi"},
	)
	if err != nil {
		return fmt.Errorf("fzf: parse options: %w", err)
	}

	options.Input = inputChan
	options.Output = outputChan

	_, err = fzf.Run(options)
	if err != nil {
		return fmt.Errorf("fzf: run: %w", err)
	}

	if len(item.Envs) != 0 {
		a.addEnvs(item.Envs, onSelect, nodes, &parentName)
	} else {
		a.execCommand(nodes, &parentName, onSelect, true)
	}

	return nil
}

func execCommand(command, selectedItems, parent string) (string, []byte, error) {
	command = os.Expand(command, func(variable string) string {
		switch variable {
		case "current":
			return selectedItems
		case "parent":
			return parent
		}

		return fmt.Sprintf("$%s", variable)
	})

	expandedCommand := command
	if strings.Contains(command, "|") {
		expandedCommand = fmt.Sprintf("set -o pipefail; %s", command)
	}
	out, err := exec.Command("sh", "-c", expandedCommand).CombinedOutput()
	if err != nil {
		return "", nil, fmt.Errorf("%s: %s: %w", command, out, err)
	}

	return command, out, nil
}

func (a *App) showError(err error) {
	a.error.
		SetText(err.Error()).
		SetFocus(0).
		SetDoneFunc(func(buttonIndex int, buttonLabel string) {
			if buttonLabel == buttonOK {
				a.pages.HidePage(pageError)
				a.app.SetRoot(a.pages, true).SetFocus(a.tree)
			}
		})
	a.pages.AddPage(pageError, a.error, true, true)
	a.pages.ShowPage(pageError)
}

func (a *App) addEnvs(envs []Env, command string, nodes []*tview.TreeNode, parentName *string) {
	inputFields := make([]*tview.InputField, len(envs))
	m := make(map[string]*tview.InputField)
	const width = 15
	maxLen := 0
	for i, env := range envs {
		inputField := tview.NewInputField().SetLabel(fmt.Sprintf("%s: ", env.Name))
		inputField.
			SetFieldWidth(env.Width).
			SetBorder(true)
		inputFields[i] = inputField
		m[env.Name] = inputField

		if len(env.Name)+env.Width > maxLen {
			maxLen = len(env.Name) + env.Width
		}
	}

	modal := func(fields []*tview.InputField, width, height int) tview.Primitive {
		flex := tview.NewFlex().SetDirection(tview.FlexRow)
		flex.AddItem(nil, 0, 1, false)
		for i, field := range fields {
			field.SetDoneFunc(func(key tcell.Key) {
				if key == tcell.KeyEnter {
					if i+1 < len(fields) {
						a.app.SetFocus(fields[i+1])
					} else {
						command = os.Expand(command, func(variable string) string {
							if v, ok := m[variable]; ok {
								return v.GetText()
							}
							return fmt.Sprintf("$%s", variable)
						})

						a.pages.HidePage(pageEnvs)
						a.execCommand(nodes, parentName, command, true)
					}
				}
			})

			flex.AddItem(field, height, 1, false)
		}
		flex.AddItem(nil, 0, 1, false)
		return tview.NewFlex().
			AddItem(nil, 0, 1, false).
			AddItem(flex, width, len(fields), true).
			AddItem(nil, 0, 1, false)
	}

	a.pages.AddPage(pageEnvs, modal(inputFields, maxLen+4, 3), true, true)
	a.pages.ShowPage(pageEnvs)
	a.app.SetFocus(inputFields[0])
}

func (a *App) addChildItems(node *tview.TreeNode, path string, parentName *string, command string, children []Item) {
	text := node.GetText()
	done := make(chan struct{})
	go func() {
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()

		i := 0
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				a.app.QueueUpdateDraw(func() {
					node.SetText(text + " " + dots[i%len(dots)])
					i += 1
				})
			}
		}
	}()

	go func() {
		defer func() {
			close(done)

			a.app.QueueUpdateDraw(func() {
				node.SetText(text)
			})
		}()

		expandedCommand, out, err := execCommand(command, text, *parentName)
		if err != nil {
			a.app.QueueUpdateDraw(func() {
				a.showError(fmt.Errorf("%s: %s: %v", expandedCommand, out, err))
			})
			return
		}

		a.app.QueueUpdateDraw(func() {
			scanner := bufio.NewScanner(bytes.NewReader(out))
			for scanner.Scan() {
				a.addChildNode(node, path, scanner.Text(), children)
			}

			if err := scanner.Err(); err != nil {
				a.showError(err)
				return
			}
		})
	}()
}

func (a *App) execCommand(nodes []*tview.TreeNode, parentName *string, command string, onSelect bool) {
	done := make(chan struct{})
	items := make([]string, len(nodes))
	for i, node := range nodes {
		text := node.GetText()
		items[i] = text
		go func() {
			ticker := time.NewTicker(100 * time.Millisecond)
			defer ticker.Stop()

			i := 0
			for {
				select {
				case <-done:
					return
				case <-ticker.C:
					a.app.QueueUpdateDraw(func() {
						node.SetText(text + " " + dots[i%len(dots)])
						i += 1
					})
				}
			}
		}()
	}

	go func() {
		defer func() {
			close(done)

			for i, node := range nodes {
				a.app.QueueUpdateDraw(func() {
					node.SetText(items[i])
				})
			}
		}()

		expandedCommand, out, err := execCommand(command, strings.Join(items, " "), *parentName)
		if err != nil {
			a.app.QueueUpdateDraw(func() {
				a.showError(fmt.Errorf("%s: %s: %v", expandedCommand, out, err))
			})
			return
		}

		a.textView.Clear()
		if _, err := io.Copy(tview.ANSIWriter(a.textView), bytes.NewBuffer(out)); err != nil {
			a.showError(err)
			return
		}

		if onSelect {
			a.textView.ScrollToEnd()
			a.app.SetFocus(a.textView)
		}
	}()
}
