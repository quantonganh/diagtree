package main

import (
	"bufio"
	"bytes"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/gdamore/tcell/v2"
	fzf "github.com/junegunn/fzf/src"
	"github.com/rivo/tview"
	"golang.org/x/term"
	"gopkg.in/yaml.v3"
)

const (
	terminalWezTerm = "wezterm"

	sep = " > "

	pageMain  = "main"
	pageError = "error"
	buttonOK  = "OK"
)

var dots = []string{"⣾ ", "⣽ ", "⣻ ", "⢿ ", "⡿ ", "⣟ ", "⣯ ", "⣷ "}

type Config struct {
	Terminal string `yaml:"terminal"`
	Root     Node   `yaml:"root"`
}

type Node struct {
	Name         string `yaml:"name,omitempty"`
	Command      string `yaml:"command,omitempty"`
	FinalCommand string `yaml:"finalCommand"`
	Children     []Node `yaml:"children,omitempty"`
}

type Reference struct {
	name       string
	parentName string
	path       string
	children   []Node
}

func main() {
	cFlag := flag.String("c", "config.yaml", "Path to the config file")
	flag.Parse()

	if err := run(cFlag); err != nil {
		log.Fatal(err)
	}
}

func run(cFlag *string) error {
	f, err := os.ReadFile(*cFlag)
	if err != nil {
		return fmt.Errorf("open file: %s", err)
	}

	var c *Config
	if err := yaml.Unmarshal(f, &c); err != nil {
		return fmt.Errorf("unmarshal: %w", err)
	}

	if c.Terminal == "" {
		c.Terminal = terminalWezTerm
	}

	pages := tview.NewPages()

	rootName := c.Root.Name
	root := tview.NewTreeNode(rootName).
		SetColor(tcell.ColorRed)
	tree := tview.NewTreeView().
		SetRoot(root).
		SetCurrentNode(root)
	tree.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		switch event.Rune() {
		case 'l':
			tree.GetCurrentNode().SetExpanded(true)
		case 'h':
			tree.GetCurrentNode().SetExpanded(false)
		}
		return event
	})

	m := make(map[string]*tview.TreeNode)

	app := tview.NewApplication()

	add := func(target *tview.TreeNode, path string, parentName *string, nodes []Node) error {
		target.SetColor(tcell.ColorRed)

		text := target.GetText()

		for _, node := range nodes {
			done := make(chan struct{})
			if node.Command != "" {
				go func() {
					ticker := time.NewTicker(100 * time.Millisecond)
					defer ticker.Stop()

					i := 0
					for {
						select {
						case <-done:
							return
						case <-ticker.C:
							app.QueueUpdateDraw(func() {
								target.SetText(text + " " + dots[i%len(dots)])
								i += 1
							})
						}
					}
				}()

				go func() {
					defer close(done)

					expandedCommand, out, err := execCommand(node.Command, target.GetText(), *parentName)
					if err != nil {
						app.QueueUpdateDraw(func() {
							showError(app, pages, tree, fmt.Sprintf("%s: %s: %v", expandedCommand, out, err))
						})
						return
					}

					app.QueueUpdateDraw(func() {
						target.SetText(text)

						scanner := bufio.NewScanner(bytes.NewReader(out))
						for scanner.Scan() {
							addChild(target, path, scanner.Text(), node.Children, m)
						}

						if err := scanner.Err(); err != nil {
							showError(app, pages, tree, err.Error())
							return
						}
					})
				}()
			}

			if node.Name != "" {
				addChild(target, path, node.Name, node.Children, m)
			}
		}

		return nil
	}

	if err := add(root, rootName, nil, c.Root.Children); err != nil {
		return fmt.Errorf("add child: %w", err)
	}

	tree.SetSelectedFunc(func(node *tview.TreeNode) {
		reference := node.GetReference()
		if reference == nil {
			return
		}
		ref, ok := reference.(*Reference)
		if !ok {
			showError(app, pages, tree, "This node has wrong reference type")
			return
		}
		children := node.GetChildren()
		if len(children) == 0 {
			if isLeafNode(ref.children) {
				finalCommand := ref.children[0].FinalCommand
				switch c.Terminal {
				case terminalWezTerm:
					finalCommand = fmt.Sprintf("pane_id=$(wezterm cli get-pane-direction right); if test -z $pane_id; then pane_id=$(wezterm cli split-pane --right --percent 75); fi; echo \"%s\" | wezterm cli send-text --pane-id $pane_id; printf \"\r\" | wezterm cli send-text --pane-id $pane_id --no-paste; wezterm cli activate-pane-direction right", ref.children[0].FinalCommand)
				}

				expandedCommand, out, err := execCommand(finalCommand, ref.name, ref.parentName)
				if err != nil {
					showError(app, pages, tree, fmt.Sprintf("%s: %s: %v", expandedCommand, out, err))
				}
			} else {
				add(node, ref.path, &ref.parentName, ref.children)
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

	app.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		if event.Rune() == '/' {
			app.Suspend(func() {
				if err := search(tree, m, height); err != nil {
					return
				}
			})
		}
		return event
	})

	pages.AddPage(pageMain, tree, true, true)
	if err := app.SetRoot(pages, true).Run(); err != nil {
		return fmt.Errorf("run: %w", err)
	}

	return nil
}

func addChild(target *tview.TreeNode, path, text string, children []Node, m map[string]*tview.TreeNode) {
	ref := &Reference{
		name:       text,
		parentName: target.GetText(),
		path:       path + sep + text,
		children:   children,
	}
	tn := tview.NewTreeNode(text).
		SetText(text).
		SetReference(ref).
		SetSelectable(true)
	target.AddChild(tn)
	m[ref.path] = tn
}

func isLeafNode(nodes []Node) bool {
	return len(nodes) == 1 && nodes[0].FinalCommand != ""
}

func search(tree *tview.TreeView, m map[string]*tview.TreeNode, height int) error {
	var paths []string
	for path, _ := range m {
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
			node, ok := m[path]
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

func showError(app *tview.Application, pages *tview.Pages, tree *tview.TreeView, msg string) {
	modal := tview.NewModal()
	modal.AddButtons([]string{buttonOK})

	modal.
		SetText(msg).
		SetFocus(0).
		SetDoneFunc(func(buttonIndex int, buttonLabel string) {
			if buttonLabel == buttonOK {
				pages.HidePage(pageError)
				app.SetRoot(pages, true).SetFocus(tree)
			}
		})
	pages.AddPage(pageError, modal, true, true)
	pages.ShowPage(pageError)
}
