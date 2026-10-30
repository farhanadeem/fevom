# Updates

Fevom checks for a new release once a day and, when there is one, shows its notes and asks
whether to install it. You can also ask at any time:

- **Fevom > Check for Updates…**, or the same item in the menu bar menu
- `fev update` in a terminal, which opens Fevom's check
- `fev update --check`, which only prints whether a newer release is out

Installing quits Fevom and opens the new version, which restarts fev: running containers
stop (those with a restart policy start again) and running machines start again, as after
`fev restart`. Your containers, images, volumes and machines are kept.

To install updates without being asked, turn on **Install updates without asking** in
Settings, App; to stop the daily check, turn off **Check for updates every day**.

Each update is checked before it is installed: the release's list and the download both carry
a signature made with fev's release key, and an update that does not match is refused.
Because Fevom is not signed with an Apple developer certificate, macOS asks again, after an
update, for what you allowed the previous version (access to folders such as Documents, and
the firewall's question if it is on).

Only a released Fevom updates itself. A Fevom built from a checkout of the source never
checks; build the new version and install it as before (see `BUILDING.md`).
