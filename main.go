package main

import (
	"bufio"
	"bytes"
	"fmt"
	"log"
	"os"
	"os/exec"

	"github.com/gdamore/tcell/v2"
	fzf "github.com/junegunn/fzf/src"
	"github.com/rivo/tview"
	"gopkg.in/yaml.v3"
)

const sep = " > "

type Config struct {
	Root Node `yaml:"root"`
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

// Show a navigable tree view of the current directory.
func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	f, err := os.ReadFile("config.yaml")
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
	tree := tview.NewTreeView().
		SetRoot(root).
		SetCurrentNode(root)

	m := make(map[string]*tview.TreeNode)

	add := func(target *tview.TreeNode, path string, nodes []Node) error {
		var childs []string
		if hasCommand(nodes) {
			node := nodes[0]
			out, err := executeCommand(nodes[0].Command)
			if err != nil {
				panic(err)
			}
			childs = append(childs, out...)
			for _, child := range childs {
				ref := &Reference{
					name:       child,
					parentName: target.GetText(),
					path:       path + sep + child,
					children:   node.Children,
				}
				tn := tview.NewTreeNode(child).
					SetText(child).
					SetReference(ref).
					SetSelectable(true)
				target.AddChild(tn)
				m[ref.path] = tn
			}
		} else {
			for _, node := range nodes {
				if node.Command != "" {
					out, err := executeCommand(node.Command)
					if err != nil {
						panic(err)
					}
					childs = append(childs, out...)
				} else {
					ref := &Reference{
						name:       node.Name,
						parentName: target.GetText(),
						path:       path + sep + node.Name,
						children:   node.Children,
					}
					tn := tview.NewTreeNode(node.Name).
						SetText(node.Name).
						SetReference(ref).
						SetSelectable(true)
					target.AddChild(tn)
					m[ref.path] = tn
				}
			}
		}

		return nil
	}

	if err := add(root, rootName, c.Root.Children); err != nil {
		return fmt.Errorf("add child: %w", err)
	}

	tree.SetSelectedFunc(func(node *tview.TreeNode) {
		reference := node.GetReference()
		if reference == nil {
			return
		}
		ref, ok := reference.(*Reference)
		if !ok {
			panic("wrong reference type")
		}
		children := node.GetChildren()
		if len(children) == 0 {
			if hasFinalCommand(ref.children) {
				command := os.Expand(ref.children[0].FinalCommand, func(variable string) string {
					if variable == "pod" || variable == "service" {
						return ref.name
					}
					if variable == "host" {
						return ref.parentName
					}
					return ""
				})
				out, err := exec.Command("sh", "-c", command).CombinedOutput()
				if err != nil {
					panic(fmt.Errorf("%s: %w", out, err))
				}
			} else {
				add(node, ref.path, ref.children)
			}
		} else {
			// Collapse if visible, expand if collapsed.
			node.SetExpanded(!node.IsExpanded())
		}
	})

	app := tview.NewApplication()
	app.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		if event.Rune() == '/' {
			app.Suspend(func() {
				if err := search(tree, m); err != nil {
					return
				}
			})
		}
		return event
	})

	if err := app.SetRoot(tree, true).Run(); err != nil {
		return fmt.Errorf("run: %w", err)
	}

	return nil
}

func hasCommand(nodes []Node) bool {
	return len(nodes) == 1 && nodes[0].Command != ""
}

func hasFinalCommand(nodes []Node) bool {
	return len(nodes) == 1 && nodes[0].FinalCommand != ""
}

func executeCommand(command string) ([]string, error) {
	out, err := exec.Command("sh", "-c", command).CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("%s: %w", out, err)
	}

	var childs []string
	scanner := bufio.NewScanner(bytes.NewReader(out))
	for scanner.Scan() {
		childs = append(childs, scanner.Text())
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan: %w", err)
	}

	return childs, nil
}

func search(tree *tview.TreeView, m map[string]*tview.TreeNode) error {
	var paths []string
	tree.GetCurrentNode().Walk(func(node, parent *tview.TreeNode) bool {
		reference := node.GetReference()
		if reference == nil {
			return false
		}
		ref, ok := reference.(*Reference)
		if !ok {
			panic("wrong reference type")
		}
		paths = append(paths, ref.path)
		return true
	})

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
			}
		}
	}()

	options, err := fzf.ParseOptions(
		true,
		[]string{"--height=50%"},
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
