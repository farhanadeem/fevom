//go:build linux

package main

import (
	"fmt"
	"strings"
)

func completionMain(args []string) int {
	if len(args) != 1 || helpWord(args[0]) {
		fmt.Fprintln(stderr, "usage: macctl completion bash|zsh|fish\n\nbash: source <(macctl completion bash)\nzsh:  source <(macctl completion zsh)\nfish: macctl completion fish | source")
		if len(args) == 1 {
			return 0
		}
		return 2
	}
	var names []string
	for _, s := range subcommands {
		names = append(names, s.name)
	}
	mac := strings.Join(names, " ")
	macctl := strings.Join(append(names, "completion", "help"), " ")
	switch args[0] {
	case "bash":
		fmt.Fprintf(stdout, `# fev: mac and macctl (macctl completion bash)
_fev_mac() {
	local cur=${COMP_WORDS[COMP_CWORD]}
	if [ "$COMP_CWORD" -eq 1 ]; then
		case ${COMP_WORDS[0]##*/} in
		macctl) COMPREPLY=($(compgen -W %q -- "$cur")) ;;
		*) COMPREPLY=($(compgen -W %q -c -- "$cur")) ;;
		esac
	elif [ "${COMP_WORDS[1]}" = completion ]; then
		COMPREPLY=($(compgen -W "bash zsh fish" -- "$cur"))
	else
		COMPREPLY=($(compgen -f -- "$cur"))
	fi
}
complete -o default -F _fev_mac mac macctl
`, macctl, mac)
	case "zsh":
		fmt.Fprintf(stdout, `#compdef mac macctl
# fev: mac and macctl (macctl completion zsh)
_fev_mac() {
	if (( CURRENT == 2 )); then
		if [[ ${words[1]:t} == macctl ]]; then
			compadd -- %s
		else
			compadd -- %s
			_command_names -e
		fi
	elif [[ ${words[2]} == completion ]]; then
		compadd -- bash zsh fish
	else
		_files
	fi
}
if (( $+functions[compdef] )); then
	compdef _fev_mac mac macctl
fi
`, macctl, mac)
	case "fish":
		fmt.Fprintf(stdout, `# fev: mac and macctl (macctl completion fish)
complete -c macctl -f -n __fish_use_subcommand -a %q
complete -c macctl -f -n '__fish_seen_subcommand_from completion' -a 'bash zsh fish'
complete -c mac -n __fish_use_subcommand -a %q
`, macctl, mac)
	default:
		fmt.Fprintf(stderr, "macctl completion: no shell %q (bash, zsh or fish)\n", args[0])
		return 2
	}
	return 0
}
