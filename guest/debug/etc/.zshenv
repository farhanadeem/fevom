case ":$PATH:" in
  *:${FEV_PROFILE:-/nix/var/fev/profile}/bin:*) ;;
  *) export PATH="${FEV_PROFILE:-/nix/var/fev/profile}/bin:/nix/var/fev/bin:$PATH" ;;
esac
