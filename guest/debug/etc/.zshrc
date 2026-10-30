setopt interactive_comments no_beep prompt_subst
setopt hist_ignore_dups hist_ignore_space share_history inc_append_history
HISTSIZE=10000
SAVEHIST=10000
HISTFILE="${HOME:-/tmp}/.zsh_history"

export LANG="${LANG:-C.UTF-8}"

_bp=${FEV_PROFILE:-/nix/var/fev/profile}
fpath=($_bp/share/zsh/site-functions $fpath)
autoload -Uz compinit && compinit -u -d "${HOME:-/tmp}/.zcompdump" 2>/dev/null
zstyle ':completion:*' menu select
zstyle ':completion:*' matcher-list 'm:{a-zA-Z}={A-Za-z}'

if command -v dircolors >/dev/null 2>&1; then
  eval "$(dircolors -b "${ZDOTDIR:-/nix/var/fev/etc}/dircolors" 2>/dev/null)"
fi
alias ls='ls --color=auto'
alias grep='grep --color=auto'
alias ll='ls -lh --color=auto'
alias la='ls -lha --color=auto'

if [ "$(id -u)" = "0" ]; then _b_c=$'%F{red}'; else _b_c=$'%F{green}'; fi
PROMPT="${_b_c}fev%f:%F{blue}%~%f %# "
RPROMPT="%F{242}${FEV_DEBUG_TARGET:-debug}%f"

for _p in \
  $_bp/share/zsh-autosuggestions/zsh-autosuggestions.zsh \
  $_bp/share/zsh-syntax-highlighting/zsh-syntax-highlighting.zsh; do
  [ -r "$_p" ] && source "$_p"
done
unset _p _b_c _bp

command_not_found_handler() {
  if (( ! $+commands[galley] )); then
    print -u2 "zsh: command not found: $1"
    return 127
  fi
  galley not-found "$@"
}
