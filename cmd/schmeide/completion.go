package main

import (
	"fmt"
	"io"
)

const completionUsage = "usage: schmeide completion bash|zsh|fish\n"

const bashCompletion = `# bash completion for schmeide
_schmeide() {
  local cur="${COMP_WORDS[COMP_CWORD]}"
  if [ "$COMP_CWORD" -eq 1 ]; then
    COMPREPLY=($(compgen -W "control runner doctor status init completion -h --help" -- "$cur"))
  elif [ "${COMP_WORDS[1]}" = completion ]; then
    COMPREPLY=($(compgen -W "bash zsh fish" -- "$cur"))
  else
    COMPREPLY=($(compgen -W "-h --help" -- "$cur"))
  fi
}
complete -F _schmeide schmeide
`

const zshCompletion = `#compdef schmeide
_schmeide() {
  local -a cmds
  cmds=(
    'control:run the control node'
    'runner:run a runner'
    'doctor:check that this machine is ready to run a runner'
    'status:show registered runners and jobs'
    'completion:print a shell completion script'
  )
  if (( CURRENT == 2 )); then
    _describe 'command' cmds
  elif [[ $words[2] == completion ]]; then
    _values 'shell' bash zsh fish
  else
    _values 'flag' -h --help
  fi
}
compdef _schmeide schmeide
`

const fishCompletion = `# fish completion for schmeide
complete -c schmeide -f
complete -c schmeide -n __fish_use_subcommand -a control -d 'Run the control node'
complete -c schmeide -n __fish_use_subcommand -a runner -d 'Run a runner'
complete -c schmeide -n __fish_use_subcommand -a doctor -d 'Check that this machine is ready'
complete -c schmeide -n __fish_use_subcommand -a status -d 'Show runners and jobs'
complete -c schmeide -n __fish_use_subcommand -a completion -d 'Print a shell completion script'
complete -c schmeide -n '__fish_seen_subcommand_from completion' -a 'bash zsh fish'
complete -c schmeide -s h -l help -d 'Show help'
`

func completionMain(args []string, stdout, stderr io.Writer) int {
	if len(args) == 1 {
		switch args[0] {
		case "bash":
			fmt.Fprint(stdout, bashCompletion)
			return 0
		case "zsh":
			fmt.Fprint(stdout, zshCompletion)
			return 0
		case "fish":
			fmt.Fprint(stdout, fishCompletion)
			return 0
		case "-h", "--help":
			fmt.Fprint(stdout, completionUsage)
			return 0
		}
	}
	fmt.Fprintf(stderr, "schmeide completion: expected one of bash, zsh, fish\n%s", completionUsage)
	return 2
}
