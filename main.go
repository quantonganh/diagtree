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
	"golang.org/x/term"
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
	Root Node `yaml:"root"`
}

type App struct {
	app         *tview.Application
	pages       *tview.Pages
	tree        *tview.TreeView
	textView    *tview.TextView
	search      *tview.InputField
	error       *tview.Modal
	pathToNodes map[string]*tview.TreeNode
	m           map[string]*tview.TextView
}

type Node struct {
	Name        string   `yaml:"name,omitempty"`
	Command     string   `yaml:"command,omitempty"`
	LeafCommand string   `yaml:"leafCommand"`
	Stream      bool     `yaml:"stream"`
	Envs        []string `yaml:"envs"`
	Children    []Node   `yaml:"children,omitempty"`
}

type Reference struct {
	node   Node
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
		m:           make(map[string]*tview.TextView),
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

	add := func(target *tview.TreeNode, path string, parentName *string, nodes []Node) error {
		target.SetColor(tcell.ColorRed)

		for _, node := range nodes {
			if node.Command != "" {
				a.execCommand(target, path, parentName, node.Command, node.Children, false)
			}

			if node.Name != "" {
				addChild(target, path, node.Name, node.Children, a.pathToNodes)
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
			if isLeafNode(ref.node.Children) {
				w := tview.ANSIWriter(a.textView)
				command := ref.node.Children[0].LeafCommand
				if ref.node.Children[0].Stream {
					command = os.Expand(command, func(variable string) string {
						switch variable {
						case "current":
							return fmt.Sprintf("%q", ref.node.Name)
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
					envs := ref.node.Children[0].Envs
					if len(envs) != 0 {
						a.addEnvs(envs, ref.node.Children[0].LeafCommand, node, ref.path, &parentName, ref.node.Children)
					} else {
						a.execCommand(node, ref.path, &parentName, ref.node.Children[0].LeafCommand, ref.node.Children, true)
					}
				}
			} else {
				add(node, ref.path, &parentName, ref.node.Children)
			}
		} else {
			// Collapse if visible, expand if collapsed.
			node.SetExpanded(!node.IsExpanded())
		}
	})

	_, height, err := term.GetSize(0)
	if err != nil {
		return fmt.Errorf("get terminal size: %w", err)
	}

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
			add(node, ref.path, &parentName, ref.node.Children)
		case '/':
			a.app.Suspend(func() {
				if err := search(tree, a.pathToNodes, height); err != nil {
					return
				}
			})
		}

		return event
	})

	a.textView.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		switch event.Rune() {
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
		case tcell.KeyCtrlH:
			a.app.SetFocus(tree)
		case tcell.KeyCtrlL:
			a.app.SetFocus(a.textView)
		case tcell.KeyCtrlJ:
			a.app.SetFocus(a.search)
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

func addChild(target *tview.TreeNode, path, text string, children []Node, m map[string]*tview.TreeNode) {
	n := Node{
		Name:     text,
		Children: children,
	}
	ref := &Reference{
		node:   n,
		parent: target,
		path:   path + sep + text,
	}
	tn := tview.NewTreeNode(text).
		SetText(text).
		SetReference(ref).
		SetSelectable(true)
	target.AddChild(tn)
	m[ref.path] = tn
}

func isLeafNode(nodes []Node) bool {
	return len(nodes) == 1 && nodes[0].LeafCommand != ""
}

func search(tree *tview.TreeView, pathToNodes map[string]*tview.TreeNode, height int) error {
	var paths []string
	for path := range pathToNodes {
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
	go func() {
		for path := range outputChan {
			node, ok := pathToNodes[path]
			if ok {
				tree.SetCurrentNode(node)
				if selectedFunc := tree.GetSelectedFunc(); selectedFunc != nil {
					selectedFunc(node)
				}
				tree.Move(height - 1)
				tree.SetCurrentNode(nil)
				tree.SetCurrentNode(node)
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

	return nil
}

func execCommand(command, current, parent string) (string, []byte, error) {
	command = os.Expand(command, func(variable string) string {
		switch variable {
		case "current":
			return fmt.Sprintf("%q", current)
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

func (a *App) addEnvs(envs []string, command string, node *tview.TreeNode, path string, parentName *string, children []Node) {
	inputFields := make([]*tview.InputField, len(envs))
	m := make(map[string]*tview.InputField)
	const width = 15
	maxLen := 0
	for i, env := range envs {
		inputField := tview.NewInputField().SetLabel(fmt.Sprintf("%s: ", env))
		inputField.
			SetFieldWidth(width).
			SetBorder(true)
		inputFields[i] = inputField
		m[env] = inputField

		if len(env) > maxLen {
			maxLen = len(env)
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
							return m[variable].GetText()
						})

						a.pages.HidePage(pageEnvs)
						a.execCommand(node, path, parentName, command, children, true)
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

	a.pages.AddPage(pageEnvs, modal(inputFields, maxLen+width+4, 3), true, true)
	a.pages.ShowPage(pageEnvs)
	a.app.SetFocus(inputFields[0])
}

func (a *App) execCommand(target *tview.TreeNode, path string, parentName *string, command string, children []Node, isLeaf bool) {
	text := target.GetText()
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
					target.SetText(text + " " + dots[i%len(dots)])
					i += 1
				})
			}
		}
	}()

	go func() {
		defer func() {
			close(done)

			a.app.QueueUpdateDraw(func() {
				target.SetText(text)
			})
		}()

		expandedCommand, out, err := execCommand(command, text, *parentName)
		if err != nil {
			a.app.QueueUpdateDraw(func() {
				a.showError(fmt.Errorf("%s: %s: %v", expandedCommand, out, err))
			})
			return
		}

		if isLeaf {
			a.textView.Clear()
			if _, err := io.Copy(tview.ANSIWriter(a.textView), bytes.NewBuffer(out)); err != nil {
				a.showError(err)
				return
			}

			a.textView.ScrollToEnd()
			a.app.SetFocus(a.textView)

		} else {
			a.app.QueueUpdateDraw(func() {
				scanner := bufio.NewScanner(bytes.NewReader(out))
				for scanner.Scan() {
					addChild(target, path, scanner.Text(), children, a.pathToNodes)
				}

				if err := scanner.Err(); err != nil {
					a.showError(err)
					return
				}
			})
		}
	}()
}
