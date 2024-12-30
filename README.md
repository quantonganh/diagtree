# fe

Fuzzy Explorer for everything: logs, configs, Kafka topics, and more.

If there is only one tool you would like to introduce to every sysadmin, devops engineer, ... anyone who live in a terminal, mine is [fzf](https://github.com/junegunn/fzf). I'm using it for everything whenever I want to search, then do something on the results:

- `brew search` then `brew install`
- `ps -ef | grep` then `kill/killall`
- open a Jira issue in the last week in the web browser
- open your project in the favorite editor
- copy a password from KeepassXC to the clipboard
- quick look Sentry issues
- ...

However, if a command takes sometime to populate a list, I want to keep fzf open after selecting:

- https://github.com/junegunn/fzf/issues/2213
- https://github.com/junegunn/fzf.vim/issues/192 

That's where Fuzzy Explorer can help.

## Installation

### Install via homebrew

```
$ brew install quantonganh/tap/fe
```

### Install via go

```
go install github.com/quantonganh/fe@latest
```

## Usage

Ensure that you're using [fish shell](https://fishshell.com/) with the [fish_title](https://fishshell.com/docs/current/cmds/fish_title.html) [function](https://github.com/fish-shell/fish-shell/blob/master/share/functions/fish_title.fish). This will allow you to see `hx` in the pane title when listing panes using `wezterm cli list --format json`:

```sh
  {
    "window_id": 0,
    "tab_id": 167,
    "pane_id": 350,
    "workspace": "default",
    "size": {
      "rows": 48,
      "cols": 175,
      "pixel_width": 2975,
      "pixel_height": 1776,
      "dpi": 144
    },
    "title": "hx . ~/C/p/helix-wezterm",
```

Install the requirements:

- [bat](https://github.com/sharkdp/bat) for file previews
- [broot](https://github.com/Canop/broot)
- [fish shell](https://fishshell.com/)
- [gh](https://cli.github.com/)
- [howdoi](https://github.com/gleitz/howdoi)
- [lazygit](https://github.com/jesseduffield/lazygit)
- [ripgrep](https://github.com/BurntSushi/ripgrep) for grep-like searching
- [tig](https://jonas.github.io/tig/)
- [tgpt](https://github.com/aandrew-me/tgpt)

Add the following into `~/.config/helix/config.toml`:

```toml
[keys.normal.space.","]
b = ":sh helix-wezterm.sh blame"
c = ":sh helix-wezterm.sh check"
e = ":sh helix-wezterm.sh explorer"
f = ":sh helix-wezterm.sh fzf"
g = ":sh helix-wezterm.sh lazygit"
o = ":sh helix-wezterm.sh open"
r = ":sh helix-wezterm.sh run"
t = ":sh helix-wezterm.sh test"
```
