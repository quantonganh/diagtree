package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gdamore/tcell/v2"
	fzf "github.com/junegunn/fzf/src"
	"github.com/lithammer/fuzzysearch/fuzzy"
	"github.com/rivo/tview"
	"golang.design/x/clipboard"
	"gopkg.in/yaml.v3"
)

const (
	sep = "/"

	pageMain  = "main"
	pageError = "error"
	buttonOK  = "OK"
	pageEnvs  = "envs"
)

var dots = []string{"⣾ ", "⣽ ", "⣻ ", "⢿ ", "⡿ ", "⣟ ", "⣯ ", "⣷ "}

type Config struct {
	Root Item `yaml:"root"`
}

type Home struct {
	app                   *tview.Application
	pages                 *tview.Pages
	treePane              *treePane
	resultPane            *resultPane
	titles                []string
	error                 *tview.Modal
	contentsByNode        map[*tview.TreeNode]map[string]string
	nodesByPath           map[string]*tview.TreeNode
	itemsByPath           map[string]Item
	nodesByTitle          map[string]*tview.TreeNode
	results               map[*tview.TreeNode]result
	currentTabIndexByNode map[*tview.TreeNode]int
}

type treePane struct {
	*tview.Flex
	searchInput *tview.InputField
	*tview.TreeView
	foundNodes       []*tview.TreeNode
	currentFocusNode *tview.TreeNode
}

type resultPane struct {
	*tview.Flex
	currentTabIndex    int
	whereOrSearchInput *tview.InputField
	outputView         *tview.TextView
	numSelections      int
}

type result struct {
	flexTitle     string
	whereOrSearch string
	tabs          []Tab
}

type Item struct {
	Name         string  `yaml:"name,omitempty"`
	OnExpand     string  `yaml:"onExpand,omitempty"`
	OnSelect     []Tab   `yaml:"onSelect"`
	OnAdd        NewData `yaml:"onAdd,omitempty"`
	Children     []Item  `yaml:"children,omitempty"`
	AutoComplete string  `yaml:"autoComplete,omitempty"`
	Where        string  `yaml:"where,omitempty"`
}

type Tab struct {
	Title   string `yaml:"title,omitempty"`
	Command string `yaml:"command"`
	Content string
	Stream  bool `yaml:"stream"`
}

type NewData struct {
	Title   string `yaml:"title,omitempty"`
	Command string `yaml:"command"`
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

	a := newHome()
	if err := a.run(cFlag); err != nil {
		log.Fatal(err)
	}
}

func newHome() *Home {
	modal := tview.NewModal()
	modal.AddButtons([]string{buttonOK})

	treePane := newTreePane()
	resultPane := newResultPane()

	search := tview.NewInputField()
	search.SetBorderPadding(0, 0, 0, 0)
	search.SetBorderColor(tview.Styles.PrimaryTextColor)
	search.SetFieldStyle(tcell.StyleDefault.Background(tview.Styles.PrimitiveBackgroundColor).Foreground(tview.Styles.PrimaryTextColor))

	return &Home{
		app:                   tview.NewApplication(),
		pages:                 tview.NewPages(),
		treePane:              treePane,
		resultPane:            resultPane,
		titles:                make([]string, 0),
		error:                 modal,
		contentsByNode:        make(map[*tview.TreeNode]map[string]string),
		nodesByPath:           make(map[string]*tview.TreeNode),
		itemsByPath:           make(map[string]Item),
		nodesByTitle:          make(map[string]*tview.TreeNode),
		results:               make(map[*tview.TreeNode]result),
		currentTabIndexByNode: make(map[*tview.TreeNode]int),
	}
}

func newTreePane() *treePane {
	tree := &treePane{
		Flex:        tview.NewFlex(),
		searchInput: tview.NewInputField(),
		TreeView:    tview.NewTreeView(),
	}

	tree.searchInput.
		SetLabel("Search: ").
		SetBorderPadding(0, 0, 0, 0)

	tree.searchInput.SetFieldStyle(tcell.StyleDefault.Background(tview.Styles.PrimitiveBackgroundColor).Foreground(tview.Styles.PrimaryTextColor))

	tree.searchInput.SetBlurFunc(func() {
		if tree.searchInput.GetText() == "" {
			tree.searchInput.SetLabelColor(tview.Styles.InverseTextColor)
		} else {
			tree.searchInput.SetLabelColor(tview.Styles.TertiaryTextColor)
		}
		tree.searchInput.SetFieldTextColor(tview.Styles.InverseTextColor)
	})

	tree.searchInput.SetChangedFunc(func(text string) {
		go tree.search(text)
	})

	tree.Flex.
		SetDirection(tview.FlexRow).
		SetBorder(true).
		SetBorderPadding(0, 0, 1, 1)

	tree.Flex.
		AddItem(tree.searchInput, 1, 0, false).
		AddItem(tree.TreeView, 0, 1, true)

	return tree
}

func newResultPane() *resultPane {
	result := &resultPane{
		Flex:               tview.NewFlex(),
		currentTabIndex:    -1,
		whereOrSearchInput: tview.NewInputField(),
		outputView:         tview.NewTextView(),
	}

	result.whereOrSearchInput.SetFieldStyle(tcell.StyleDefault.Background(tview.Styles.PrimitiveBackgroundColor).Foreground(tview.Styles.PrimaryTextColor))

	result.outputView.
		SetDynamicColors(true).
		SetRegions(true)

	result.Flex.
		SetDirection(tview.FlexRow).
		SetBorder(true).
		SetBorderPadding(0, 0, 1, 1)

	result.Flex.
		AddItem(result.whereOrSearchInput, 3, 0, false).
		AddItem(result.outputView, 0, 1, false)

	return result
}

func (h *Home) run(cFlag *string) error {
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
	tree := h.treePane.SetRoot(root).SetCurrentNode(root)

	h.add(root, rootName, nil, c.Root.Children)

	var (
		currentCmd   *exec.Cmd
		cancelStream context.CancelFunc
	)
	tree.SetSelectedFunc(func(node *tview.TreeNode) {
		reference := node.GetReference()
		if reference == nil {
			return
		}
		ref, ok := reference.(*Reference)
		if !ok {
			h.showError(errors.New("This node has wrong reference type"))
			return
		}

		if cancelStream != nil {
			cancelStream()
			cancelStream = nil
		}

		if currentCmd != nil && currentCmd.Process != nil {
			pgid, err := syscall.Getpgid(currentCmd.Process.Pid)
			if err == nil {
				syscall.Kill(-pgid, syscall.SIGKILL)
			} else {
				if err := currentCmd.Process.Kill(); err != nil {
					h.showError(err)
					return
				}
			}
		}

		children := node.GetChildren()
		if len(children) != 0 {
			// Collapse if visible, expand if collapsed.
			node.SetExpanded(!node.IsExpanded())
		} else {
			parentName := ref.parent.GetText()
			if len(ref.item.OnSelect) == 0 {
				h.add(node, ref.path, &parentName, ref.item.Children)
			} else {
				w := tview.ANSIWriter(h.resultPane.outputView)
				result, ok := h.results[node]
				if ok {
					h.resultPane.outputView.Clear()
					currentTab := h.currentTabIndexByNode[node]
					h.setFlexTitle([]*tview.TreeNode{node})
					h.resultPane.whereOrSearchInput.SetText(result.whereOrSearch)
					h.setTextViewTitle([]*tview.TreeNode{node})
					if _, err := io.Copy(tview.ANSIWriter(h.resultPane.outputView), bytes.NewBuffer([]byte(result.tabs[currentTab].Content))); err != nil {
						h.showError(err)
						return
					}
					h.app.SetFocus(h.resultPane.outputView)
				} else {
					h.currentTabIndexByNode[node] = 0
					h.appendFlexTitle([]*tview.TreeNode{node}, "")
					h.resultPane.whereOrSearchInput.SetText("")
					h.setTextViewTitle([]*tview.TreeNode{node})
					h.resultPane.currentTabIndex++
					h.setContent([]*tview.TreeNode{node}, &parentName)
					h.app.SetFocus(h.resultPane.outputView)
					title := h.getTitleByNodes([]*tview.TreeNode{node}, "")
					h.nodesByTitle[title] = node
					for _, tab := range ref.item.OnSelect {
						command := tab.Command
						if tab.Stream {
							var ctx context.Context
							ctx, cancelStream = context.WithCancel(context.Background())
							go func(ctx context.Context) {
								command = os.Expand(command, func(variable string) string {
									switch variable {
									case "current":
										return fmt.Sprintf("%q", ref.item.Name)
									case "parent":
										return parentName
									}

									return fmt.Sprintf("$%s", variable)
								})

								expandedCommand := command
								if strings.Contains(command, "|") {
									expandedCommand = fmt.Sprintf("set -o pipefail; %s", command)
								}
								cmd := exec.Command("sh", "-c", expandedCommand)
								cmd.SysProcAttr = &syscall.SysProcAttr{
									Setpgid: true,
								}
								currentCmd = cmd
								stdout, err := cmd.StdoutPipe()
								if err != nil {
									h.showError(err)
									return
								}

								if err := cmd.Start(); err != nil {
									h.showError(err)
									return
								}

								stdoutReader := bufio.NewScanner(stdout)
								for {
									select {
									case <-ctx.Done():
										return
									default:
										if stdoutReader.Scan() {
											if json.Valid(stdoutReader.Bytes()) {
												var data map[string]any
												if err := json.Unmarshal(stdoutReader.Bytes(), &data); err != nil {
													h.showError(err)
													return
												}

												prettyJSON, err := json.MarshalIndent(data, "", "  ")
												if err != nil {
													h.showError(err)
													return
												}

												fmt.Fprintln(w, string(prettyJSON))
											} else {
												fmt.Fprintln(w, stdoutReader.Text())
											}
										}
									}
								}
							}(ctx)
						}
					}
				}
			}
		}
	})

	h.treePane.TreeView.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		switch event.Rune() {
		case rune(tcell.KeyTab):
			if len(h.titles) != 0 {
				h.app.SetFocus(h.resultPane.outputView)
			}
		// case 'a':
		// 	reference := tree.GetCurrentNode().GetReference()
		// 	if reference == nil {
		// 		return nil
		// 	}

		// 	ref, ok := reference.(*Reference)
		// 	if !ok {
		// 		return nil
		// 	}

		// 	newData := ref.item.Children[0].OnAdd
		// 	h.addNewItem(newData)
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
			h.add(node, ref.path, &parentName, ref.item.Children)
		case '/':
			h.app.SetFocus(h.treePane.searchInput)
		case 'n':
			h.treePane.gotoNextFoundNode()
		case 'p':
			h.treePane.gotoPreviousFoundNode()
		case 'f':
			h.app.Suspend(func() {
				if err := h.fzf(); err != nil {
					return
				}
			})
		}

		return event
	})

	h.treePane.searchInput.SetDoneFunc(func(key tcell.Key) {
		switch key {
		case tcell.KeyEnter:
		case tcell.KeyEscape:
			h.treePane.searchInput.SetText("")
		}

		h.app.SetFocus(h.treePane.TreeView)
		// h.onSelect(h.treePane.GetCurrentNode())
	})

	h.resultPane.Flex.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		n := len(h.titles)
		if n == 0 {
			return nil
		}

		switch event.Rune() {
		case ']':
			if err := h.gotoNextTab(); err != nil {
				h.showError(err)
				return nil
			}
		case '[':
			if err := h.gotoPreviousTab(); err != nil {
				h.showError(err)
				return nil
			}
			// case 'c':
			// 	i := h.resultPane.currentTabIndex
			// 	if i < 0 || i >= n {
			// 		return nil
			// 	}

			// 	if n == 1 {
			// 		node, ok := h.titleToNodes[h.titles[0]]
			// 		if ok {
			// 			delete(h.results, node)
			// 		}
			// 		h.titles = nil
			// 		h.resultPane.currentTabIndex = -1
			// 		h.resultPane.Flex.SetTitle("")
			// 		h.resultPane.whereOrSearchInput.SetLabel("")
			// 		h.resultPane.whereOrSearchInput.SetBorder(false)
			// 		h.resultPane.outputView.SetTitle("")
			// 		h.resultPane.outputView.SetText("")
			// 		h.resultPane.outputView.SetBorder(false)
			// 		h.app.SetFocus(h.treePane.TreeView)
			// 		return nil
			// 	}

			// 	nodeToDelete := h.titleToNodes[h.titles[i]]
			// 	h.titles = append(h.titles[:i], h.titles[i+1:]...)
			// 	delete(h.results, nodeToDelete)

			// 	var nextIndex int
			// 	if i >= len(h.titles) {
			// 		nextIndex = len(h.titles) - 1
			// 	} else {
			// 		nextIndex = i
			// 	}

			// 	h.resultPane.currentTabIndex = nextIndex
			// 	h.resultPane.Flex.SetTitle(strings.Join(h.titles, sep))

			// 	if err := h.gotoTab(nextIndex); err != nil {
			// 		h.showError(err)
			// 		return nil
			// 	}
		}
		return event
	})

	h.resultPane.outputView.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		switch event.Rune() {
		case rune(tcell.KeyTab):
			h.app.SetFocus(h.treePane.TreeView)
		case '/':
			h.app.SetFocus(h.resultPane.whereOrSearchInput)

			reference := h.treePane.GetCurrentNode().GetReference()
			if reference == nil {
				return nil
			}

			ref, ok := reference.(*Reference)
			if !ok {
				return nil
			}

			if ref.item.AutoComplete != "" {
				var mu sync.Mutex
				prefixMap := make(map[string][]string)
				h.resultPane.whereOrSearchInput.SetAutocompleteFunc(func(currentText string) []string {
					prefix := strings.TrimSpace(strings.ToLower(currentText))
					if prefix == "" || strings.Contains(currentText, " ") {
						return nil
					}

					mu.Lock()
					defer mu.Unlock()
					entries, ok := prefixMap[prefix]
					if ok {
						return entries
					}

					go func() {
						expandedCommand, out, err := execCommand(ref.item.AutoComplete, h.treePane.GetCurrentNode().GetText(), ref.parent.GetText())
						if err != nil {
							h.app.QueueUpdateDraw(func() {
								h.showError(fmt.Errorf("%s: %s: %v", expandedCommand, out, err))
							})
							return
						}

						var words []string
						scanner := bufio.NewScanner(bytes.NewReader(out))
						for scanner.Scan() {
							words = append(words, scanner.Text())
						}
						if err := scanner.Err(); err != nil {
							h.app.QueueUpdateDraw(func() {
								h.showError(err)
							})
							return
						}

						entries := make([]string, 0, len(words))
					OuterLoop:
						for _, word := range words {
							for _, entry := range entries {
								if strings.EqualFold(entry, word) {
									continue OuterLoop
								}
							}
							entries = append(entries, word)
						}

						mu.Lock()
						prefixMap[prefix] = entries
						mu.Unlock()

						h.app.QueueUpdateDraw(func() {
							h.resultPane.whereOrSearchInput.Autocomplete()
						})
					}()

					return nil
				})
			}
		case 'n':
			regionIDs := h.resultPane.outputView.GetHighlights()
			if len(regionIDs) > 0 {
				i, _ := strconv.Atoi(regionIDs[0])
				i = (i + 1) % h.resultPane.numSelections
				h.resultPane.outputView.Highlight(strconv.Itoa(i)).ScrollToHighlight()
			}
		case 'p':
			regionIDs := h.resultPane.outputView.GetHighlights()
			if len(regionIDs) > 0 {
				i, _ := strconv.Atoi(regionIDs[0])
				i = (i - 1 + h.resultPane.numSelections) % h.resultPane.numSelections
				h.resultPane.outputView.Highlight(strconv.Itoa(i)).ScrollToHighlight()
			}
		case 'y':
			if err := clipboard.Init(); err != nil {
				h.showError(err)
				return nil
			}
			clipboard.Write(clipboard.FmtText, []byte(h.resultPane.outputView.GetText(true)))
		case 'l':
			h.resultPane.outputView.Clear()
			node := h.treePane.GetCurrentNode()
			reference := node.GetReference()
			if reference == nil {
				return nil
			}

			ref, ok := reference.(*Reference)
			if !ok {
				return nil
			}

			tabs := ref.item.OnSelect
			h.currentTabIndexByNode[node] = (h.currentTabIndexByNode[node] + 1) % len(tabs)
			h.setTextViewTitle([]*tview.TreeNode{node})

			m, ok := h.contentsByNode[node]
			if !ok {
				return nil
			}

			viewTitle := ref.item.OnSelect[h.currentTabIndexByNode[node]].Title
			content, ok := m[viewTitle]
			if ok {
				if _, err := io.Copy(tview.ANSIWriter(h.resultPane.outputView), bytes.NewBuffer([]byte(content))); err != nil {
					h.showError(err)
					return nil
				}
			} else {
				parentName := ref.parent.GetText()
				h.setContent([]*tview.TreeNode{node}, &parentName)
			}
		case 'h':
			h.resultPane.outputView.Clear()
			node := h.treePane.GetCurrentNode()
			reference := node.GetReference()
			if reference == nil {
				return nil
			}

			ref, ok := reference.(*Reference)
			if !ok {
				return nil
			}

			tabs := ref.item.OnSelect
			h.currentTabIndexByNode[node] = (h.currentTabIndexByNode[node] - 1 + len(tabs)) % len(tabs)
			h.setTextViewTitle([]*tview.TreeNode{node})

			m, ok := h.contentsByNode[node]
			if !ok {
				return nil
			}

			viewTitle := ref.item.OnSelect[h.currentTabIndexByNode[node]].Title
			content, ok := m[viewTitle]
			if ok {
				if _, err := io.Copy(tview.ANSIWriter(h.resultPane.outputView), bytes.NewBuffer([]byte(content))); err != nil {
					h.showError(err)
					return nil
				}
			} else {
				parentName := ref.parent.GetText()
				h.setContent([]*tview.TreeNode{node}, &parentName)
			}
		case rune(tcell.KeyEsc):
			h.app.SetFocus(h.resultPane.outputView)
		}
		return event
	})

	h.resultPane.whereOrSearchInput.SetFocusFunc(func() {
		if h.resultPane.whereOrSearchInput.GetText() == "" {
			h.resultPane.whereOrSearchInput.Autocomplete()
		}
	})

	h.resultPane.outputView.SetChangedFunc(func() {
		h.app.Draw()
		h.resultPane.outputView.ScrollToEnd()
	})

	h.app.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		switch event.Key() {
		case tcell.KeyEsc:
			// a.pages.SwitchToPage(pageMain)
		}
		return event
	})

	h.resultPane.whereOrSearchInput.SetDoneFunc(func(key tcell.Key) {
		switch key {
		case tcell.KeyEnter:
			searchText := h.resultPane.whereOrSearchInput.GetText()

			node := h.treePane.GetCurrentNode()
			reference := node.GetReference()
			if reference == nil {
				return
			}

			ref, ok := reference.(*Reference)
			if !ok {
				return
			}
			if ref.item.Where != "" {
				title := h.resultPane.outputView.GetTitle()
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
							h.app.QueueUpdateDraw(func() {
								h.resultPane.outputView.SetTitle(title + " " + dots[i%len(dots)])
								i += 1
							})
						}
					}
				}()

				go func() {
					defer func() {
						close(done)

						h.app.QueueUpdateDraw(func() {
							h.resultPane.outputView.SetTitle(title)
							h.app.SetFocus(h.resultPane.outputView)
						})
					}()

					command := os.Expand(ref.item.Where, func(variable string) string {
						switch variable {
						case "current":
							return node.GetText()
						case "condition":
							return searchText
						}

						return fmt.Sprintf("$%s", variable)
					})

					expandedCommand := command
					if strings.Contains(command, "|") {
						expandedCommand = fmt.Sprintf("set -o pipefail; %s", command)
					}
					out, err := exec.Command("sh", "-c", expandedCommand).CombinedOutput()
					if err != nil {
						h.showError(fmt.Errorf("%s: %s: %w", command, out, err), []tview.Primitive{h.resultPane.whereOrSearchInput}...)
						return
					}

					h.resultPane.outputView.Clear()
					h.setTextViewTitle([]*tview.TreeNode{node})
					if _, err := io.Copy(tview.ANSIWriter(h.resultPane.outputView), bytes.NewBuffer(out)); err != nil {
						h.showError(err, []tview.Primitive{h.resultPane.whereOrSearchInput}...)
						return
					}

					r, ok := h.results[node]
					if ok {
						r.whereOrSearch = searchText
						h.results[node] = r
					}
				}()
			} else {
				lowerSearchText := strings.ToLower(searchText)
				outputText := h.resultPane.outputView.GetText(false)

				var builder strings.Builder
				i := 0
				for i < len(outputText) {
					idx := strings.Index(strings.ToLower(outputText[i:]), lowerSearchText)
					if idx == -1 {
						builder.WriteString(outputText[i:])
						break
					}

					matchStart := i + idx
					matchEnd := matchStart + len(searchText)

					builder.WriteString(outputText[i:matchStart])

					match := outputText[matchStart:matchEnd]
					builder.WriteString(fmt.Sprintf(`["%d"]%s[""]`, h.resultPane.numSelections, match))
					h.resultPane.numSelections++

					i = matchEnd
				}

				if h.resultPane.numSelections > 0 {
					h.resultPane.outputView.Clear()
					h.resultPane.outputView.SetText(builder.String())
					h.app.SetFocus(h.resultPane.outputView)

					currentSelection := h.resultPane.outputView.GetHighlights()
					if len(currentSelection) > 0 {
						h.resultPane.outputView.Highlight()
					} else {
						h.resultPane.outputView.Highlight("0").ScrollToHighlight()
					}
				}
			}
		case tcell.KeyEsc:
		}
	})

	mainFlex := tview.NewFlex().SetDirection(tview.FlexColumn).
		AddItem(h.treePane.Flex, 0, 3, true).
		AddItem(h.resultPane.Flex, 0, 7, false)

	h.pages.AddPage(pageMain, mainFlex, true, true)
	if err := h.app.SetRoot(h.pages, true).SetFocus(tree).Run(); err != nil {
		return fmt.Errorf("run: %w", err)
	}

	return nil
}

func (h *Home) addChild(node *tview.TreeNode, path, name string, children []Item) {
	childItem := Item{
		Name:     name,
		Children: children,
	}
	if len(children) > 0 {
		childItem.OnSelect = children[0].OnSelect
		childItem.AutoComplete = children[0].AutoComplete
		childItem.Where = children[0].Where
	}

	ref := &Reference{
		item:   childItem,
		parent: node,
		path:   path + sep + name,
	}
	childNode := tview.NewTreeNode(name).
		SetText(name).
		SetReference(ref).
		SetSelectable(true)
	node.AddChild(childNode)
	h.nodesByPath[ref.path] = childNode
	h.itemsByPath[ref.path] = childItem
}

func (h *Home) fzf() error {
	var paths []string
	for path := range h.nodesByPath {
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
		query      string
		gotQuery   bool
		nodes      []*tview.TreeNode
		items      []Item
		parentName string
		item       Item
		onSelect   = make([]Tab, 0)
	)
	go func() {
		for line := range outputChan {
			if !gotQuery {
				query = line
				gotQuery = true
				continue
			}

			node, ok := h.nodesByPath[line]
			if ok {
				nodes = append(nodes, node)

				if parentName == "" {
					reference := node.GetReference()
					if reference == nil {
						return
					}
					ref, ok := reference.(*Reference)
					if !ok {
						h.showError(errors.New("This node has wrong reference type"))
						return
					}
					parentName = ref.parent.GetText()
				}
			}

			item, ok = h.itemsByPath[line]
			if ok {
				items = append(items, item)

				if len(onSelect) == 0 {
					onSelect = item.OnSelect
				}
			}
		}
	}()

	options, err := fzf.ParseOptions(
		true,
		[]string{"--height=50%", "--multi", "--print-query"},
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

	if len(nodes) == 0 {
		return nil
	}

	if len(nodes) >= 2 {
		level := nodes[0].GetLevel()
		for _, node := range nodes[1:] {
			if node.GetLevel() != level {
				err := errors.New("Cannot select nodes at different levels.")
				h.showError(err)
				return err
			}
		}
	}

	numParents := 0
	for _, item := range items {
		if len(item.OnSelect) == 0 {
			numParents++
		}
	}
	if numParents >= 2 {
		err := errors.New("Cannot select multiple parent nodes.")
		h.showError(err)
		return err
	}

	if numParents == 0 {
		if len(nodes) > 1 {
			h.treePane.SetCurrentNode(nodes[0])
			h.appendFlexTitle(nodes, query)
			h.setTextViewTitle(nodes)
			h.app.SetFocus(h.resultPane.outputView)
			h.resultPane.currentTabIndex++
			h.setContent(nodes, &parentName)
			h.nodesByTitle[h.getTitleByNodes(nodes, query)] = nodes[0]
		} else {
			h.onSelect(nodes[0])
		}
	} else {
		node := nodes[0]
		h.treePane.SetCurrentNode(node)
		reference := node.GetReference()
		if reference == nil {
			return nil
		}
		ref, ok := reference.(*Reference)
		if !ok {
			err := errors.New("This node has wrong reference type")
			h.showError(err)
			return err
		}
		h.add(node, ref.path, &parentName, ref.item.Children)
	}

	return nil
}

func (h *Home) add(node *tview.TreeNode, path string, parentName *string, items []Item) {
	node.SetColor(tcell.ColorRed)

	for _, item := range items {
		if item.OnExpand != "" {
			h.addChildren(node, path, parentName, item.OnExpand, item.Children)
		}

		if item.Name != "" {
			h.addChild(node, path, item.Name, item.Children)
		}
	}
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

func (h *Home) showError(err error, primitives ...tview.Primitive) {
	h.error.
		SetText(err.Error()).
		SetFocus(0).
		SetDoneFunc(func(buttonIndex int, buttonLabel string) {
			if buttonLabel == buttonOK {
				h.pages.HidePage(pageError)
				if len(primitives) > 0 {
					h.app.SetRoot(h.pages, true).SetFocus(primitives[0])
				}
			}
		})
	h.pages.AddPage(pageError, h.error, true, true)
	h.pages.ShowPage(pageError)
}

func (h *Home) addChildren(node *tview.TreeNode, path string, parentName *string, command string, children []Item) {
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
				h.app.QueueUpdateDraw(func() {
					node.SetText(text + " " + dots[i%len(dots)])
					i += 1
				})
			}
		}
	}()

	go func() {
		defer func() {
			close(done)

			h.app.QueueUpdateDraw(func() {
				node.SetText(text)
			})
		}()

		expandedCommand, out, err := execCommand(command, text, *parentName)
		if err != nil {
			h.app.QueueUpdateDraw(func() {
				h.showError(fmt.Errorf("%s: %s: %v", expandedCommand, out, err))
			})
			return
		}

		var lines []string
		scanner := bufio.NewScanner(bytes.NewReader(out))
		for scanner.Scan() {
			lines = append(lines, scanner.Text())
		}
		if err := scanner.Err(); err != nil {
			h.app.QueueUpdateDraw(func() {
				h.showError(err)
			})
			return
		}

		for _, line := range lines {
			h.addChild(node, path, line, children)
		}

		h.app.QueueUpdateDraw(func() {
			// node.SetExpanded(true)
		})
	}()
}

func (h *Home) addNewItem(data NewData) {
	inputField := tview.NewInputField()
	inputField.
		SetFieldWidth(64).
		SetBorder(true).
		SetTitle(data.Title)

	modal := func(field *tview.InputField, width, height int) tview.Primitive {
		flex := tview.NewFlex().SetDirection(tview.FlexRow)
		flex.AddItem(nil, 0, 1, false)
		field.SetDoneFunc(func(key tcell.Key) {
			if key == tcell.KeyEnter {
				defer h.pages.HidePage(pageEnvs)

				command := os.Expand(data.Command, func(variable string) string {
					switch variable {
					case "data":
						return fmt.Sprintf("%s", inputField.GetText())
					}

					return fmt.Sprintf("$%s", variable)
				})

				expandedCommand := command
				if strings.Contains(command, "|") {
					expandedCommand = fmt.Sprintf("set -o pipefail; %s", command)
				}
				out, err := exec.Command("sh", "-c", expandedCommand).CombinedOutput()
				if err != nil {
					h.showError(fmt.Errorf("%s: %s: %w", command, out, err))
					return
				}
				h.resultPane.outputView.SetText(string(out))
			}
		})

		flex.AddItem(field, height, 1, false)

		flex.AddItem(nil, 0, 1, false)
		return tview.NewFlex().
			AddItem(nil, 0, 1, false).
			AddItem(flex, width, 1, true).
			AddItem(nil, 0, 1, false)
	}

	h.pages.AddPage(pageEnvs, modal(inputField, 64, 3), true, true)
	h.pages.ShowPage(pageEnvs)
	h.app.SetFocus(inputField)
}

func (h *Home) onSelect(node *tview.TreeNode) {
	reference := node.GetReference()
	if reference == nil {
		return
	}
	ref, ok := reference.(*Reference)
	if !ok {
		h.showError(errors.New("This node has wrong reference type"))
		return
	}
	parentName := ref.parent.GetText()

	result, ok := h.results[node]
	if ok {
		h.resultPane.outputView.Clear()
		currentTab := h.currentTabIndexByNode[node]
		h.setFlexTitle([]*tview.TreeNode{node})
		h.resultPane.whereOrSearchInput.SetText(result.whereOrSearch)
		h.setTextViewTitle([]*tview.TreeNode{node})
		if _, err := io.Copy(tview.ANSIWriter(h.resultPane.outputView), bytes.NewBuffer([]byte(result.tabs[currentTab].Content))); err != nil {
			h.showError(err)
			return
		}
		h.app.SetFocus(h.resultPane.outputView)
	} else {
		h.appendFlexTitle([]*tview.TreeNode{node}, "")
		h.resultPane.whereOrSearchInput.SetText("")
		h.setTextViewTitle([]*tview.TreeNode{node})
		h.app.SetFocus(h.resultPane.outputView)
		title := h.getTitleByNodes([]*tview.TreeNode{node}, "")
		h.nodesByTitle[title] = node
		h.resultPane.currentTabIndex++
		h.setContent([]*tview.TreeNode{node}, &parentName)
	}
}

func (h *Home) appendFlexTitle(nodes []*tview.TreeNode, query string) {
	currentTitle := h.getTitleByNodes(nodes, query)
	h.titles = append(h.titles, currentTitle)

	var sb strings.Builder
	for i, title := range h.titles {
		if title == currentTitle {
			sb.WriteString("[green:-]" + title + "[-:-:-]")
		} else {
			sb.WriteString(title)
		}
		if i != len(h.titles)-1 {
			sb.WriteString(" | ")
		}
	}
	h.resultPane.Flex.SetTitle(sb.String())
}

func (h *Home) setFlexTitle(nodes []*tview.TreeNode) {
	nodeTitle := h.getTitleByNodes(nodes, "")
	var sb strings.Builder
	for i, title := range h.titles {
		if title == nodeTitle || isFuzzyTitle(title, nodeTitle) {
			sb.WriteString("[green:-]" + title + "[-:-:-]")
		} else {
			sb.WriteString(title)
		}
		if i != len(h.titles)-1 {
			sb.WriteString(" | ")
		}
	}
	h.resultPane.Flex.SetTitle(sb.String())
}

func isFuzzyTitle(title, nodeTitle string) bool {
	parts := strings.Split(title, sep)
	nodeParts := strings.Split(nodeTitle, sep)
	n := len(nodeParts)
	return strings.Join(parts[:n-1], sep) == strings.Join(nodeParts[:n-1], sep) && strings.Contains(nodeParts[n-1], parts[n-1])
}

func (h *Home) setTextViewTitle(nodes []*tview.TreeNode) {
	node := nodes[0]
	reference := node.GetReference()
	if reference == nil {
		return
	}
	ref, ok := reference.(*Reference)
	if !ok {
		h.showError(errors.New("This node has wrong reference type"))
		return
	}

	tabs := ref.item.OnSelect
	var sb strings.Builder
	for i, tab := range tabs {
		if tab.Title != "" {
			if i == h.currentTabIndexByNode[node] {
				sb.WriteString("[green:-]" + tab.Title + "[-:-:-]")
			} else {
				sb.WriteString(tab.Title)
			}
			if i != len(tabs)-1 {
				sb.WriteString(" | ")
			}
		}
	}
	h.resultPane.whereOrSearchInput.SetBorder(true)
	var label string
	if ref.item.Where != "" {
		label = "WHERE "
	} else {
		label = "Search: "
	}
	h.resultPane.whereOrSearchInput.SetLabel(label)
	h.resultPane.outputView.SetBorder(true)
	h.resultPane.outputView.SetTitle(sb.String())
}

func (h *Home) setContent(nodes []*tview.TreeNode, parentName *string) {
	done := make(chan struct{})
	items := make([]string, len(nodes))
	title := h.resultPane.GetTitle()
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
					h.app.QueueUpdateDraw(func() {
						node.SetText(text + " " + dots[i%len(dots)])
						h.resultPane.SetTitle(title + " " + dots[i%len(dots)])
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
				h.app.QueueUpdateDraw(func() {
					node.SetText(items[i])
				})
			}
			h.app.QueueUpdateDraw(func() {
				h.resultPane.SetTitle(title)
			})
		}()

		node := nodes[0]
		reference := node.GetReference()
		if reference == nil {
			return
		}
		ref, ok := reference.(*Reference)
		if !ok {
			h.showError(errors.New("This node has wrong reference type"))
			return
		}

		currentTabIndex := h.currentTabIndexByNode[node]
		tab := ref.item.OnSelect[currentTabIndex]
		expandedCommand, out, err := execCommand(tab.Command, strings.Join(items, " "), *parentName)
		if err != nil {
			h.app.QueueUpdateDraw(func() {
				h.showError(fmt.Errorf("%s: %s: %v", expandedCommand, out, err))
			})
			return
		}
		tab.Content = string(out)
		tabs := h.results[node].tabs
		h.results[node] = result{
			flexTitle: title,
			tabs:      append(tabs, tab),
		}

		m, ok := h.contentsByNode[node]
		if !ok {
			m = make(map[string]string)
		}
		m[tab.Title] = tab.Content
		h.contentsByNode[node] = m

		h.resultPane.outputView.Clear()
		if _, err := io.Copy(tview.ANSIWriter(h.resultPane.outputView), bytes.NewBuffer([]byte(h.results[nodes[0]].tabs[currentTabIndex].Content))); err != nil {
			h.showError(err)
			return
		}
	}()
}

func (h *Home) getTitleByNodes(nodes []*tview.TreeNode, query string) string {
	node := nodes[0]
	reference := node.GetReference()
	if reference == nil {
		return ""
	}
	ref, ok := reference.(*Reference)
	if !ok {
		err := errors.New("This node has wrong reference type")
		h.showError(err)
		return ""
	}

	parts := strings.Split(ref.path, sep)
	n := len(parts)
	var title string
	if len(nodes) > 1 {
		title = parts[n-4 : n-3][0] + sep + string(parts[n-3 : n-2][0][0]) + sep + parts[n-2 : n-1][0] + sep + query
	} else {
		title = parts[n-4 : n-3][0] + sep + string(parts[n-3 : n-2][0][0]) + sep + strings.Join(parts[n-2:], sep)
	}
	return strings.ToLower(title)
}

func (h *Home) gotoNextTab() error {
	n := len(h.titles)
	h.resultPane.currentTabIndex = (h.resultPane.currentTabIndex + 1) % n

	return h.gotoTab(h.resultPane.currentTabIndex)
}

func (h *Home) gotoPreviousTab() error {
	n := len(h.titles)
	h.resultPane.currentTabIndex = (h.resultPane.currentTabIndex - 1 + n) % n

	return h.gotoTab(h.resultPane.currentTabIndex)
}

func (h *Home) gotoTab(index int) error {
	node, ok := h.nodesByTitle[h.titles[index]]
	if !ok {
		return nil
	}

	reference := node.GetReference()
	if reference == nil {
		return nil
	}
	ref, ok := reference.(*Reference)
	if !ok {
		err := errors.New("This node has wrong reference type")
		h.showError(err)
		return err
	}

	if ref.parent != nil && !ref.parent.IsExpanded() {
		ref.parent.SetExpanded(true)
	}

	h.treePane.SetCurrentNode(node)
	tabs := h.results[node].tabs
	h.resultPane.outputView.Clear()
	h.setFlexTitle([]*tview.TreeNode{node})
	result, ok := h.results[node]
	if ok {
		h.resultPane.whereOrSearchInput.SetText(result.whereOrSearch)
	}
	h.setTextViewTitle([]*tview.TreeNode{node})
	if _, err := io.Copy(tview.ANSIWriter(h.resultPane.outputView), bytes.NewBuffer([]byte(tabs[h.currentTabIndexByNode[node]].Content))); err != nil {
		return err
	}

	return nil
}

func (t *treePane) search(searchText string) {
	root := t.GetRoot()
	lowerSearchText := strings.ToLower(searchText)
	if lowerSearchText == "" {
		root.Walk(func(_, parent *tview.TreeNode) bool {
			if parent != nil && parent != root && parent.IsExpanded() {
				parent.SetExpanded(false)
			}

			return true
		})
	}

	t.foundNodes = make([]*tview.TreeNode, 0)
	root.Walk(func(node, parent *tview.TreeNode) bool {
		nodeText := strings.ToLower(node.GetText())
		if fuzzy.Match(lowerSearchText, nodeText) {
			if parent != nil {
				parent.SetExpanded(true)
			}
			t.SetCurrentNode(node)
			t.foundNodes = append(t.foundNodes, node)
			t.currentFocusNode = node
		}

		return true
	})
}

func (t *treePane) gotoNextFoundNode() {
	for i, node := range t.foundNodes {
		if node == t.currentFocusNode {
			newFocusNode := t.foundNodes[(i+1)%len(t.foundNodes)]
			t.SetCurrentNode(newFocusNode)
			t.currentFocusNode = newFocusNode
			break
		}
	}
}

func (t *treePane) gotoPreviousFoundNode() {
	n := len(t.foundNodes)
	for i, node := range t.foundNodes {
		if node == t.currentFocusNode {
			newFocusNode := t.foundNodes[(i-1+n)%n]
			t.SetCurrentNode(newFocusNode)
			t.currentFocusNode = newFocusNode
			break
		}
	}
}
