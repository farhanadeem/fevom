# ~/Fevom: your Linux files in Finder

While fev runs, the folder `Fevom` in your home folder shows the files of fev's Linux
machines and Docker objects. Open it in Finder, in Terminal, or in any app. Nothing in it
takes space on your Mac: the files live in fev's Linux VM.

| Where | What | Can you change it? |
|---|---|---|
| `~/Fevom/<machine>/` | each Linux machine's whole file system, running or stopped | yes |
| `~/Fevom/docker/volumes/<name>/` | each Docker volume's files | yes |
| `~/Fevom/docker/containers/<name>/` | each running container's files | yes |
| `~/Fevom/docker/images/<tag>/` | each image's files | no: images are read-only |

A container that stops leaves the folder; it is back when the container runs again. Volumes, machines and images stay.
Isolated machines are never shown.

Only you can open the folder's files: other accounts on this Mac are refused at `~/Fevom`
itself, even if they mount it themselves. Root (`sudo`, and Finder's helper that deletes
files) acts as you there, as it can read your home folder anyway.

Files you create in a machine's folder belong to that machine's default user; in Docker's
folders, to root. You can read and write files of any Linux user from the Mac.

## When fev is stopped

`~/Fevom` is then an ordinary folder that holds only `README.txt`, which says fev is
stopped, and you cannot add files to it. When fev starts, the files appear again.

If `~/Fevom` already holds files of your own when fev starts, fev does not cover them:
it shows nothing there and says why in `fev status`. Move your files elsewhere, and fev
shows its files at its next start.

## Writes are kept within 30 seconds

What you write through `~/Fevom` reaches the VM at once, but the VM writes it to its disk
within 30 seconds, as Linux does. If the VM stops without warning in those 30 seconds (it
crashes, or fev itself is killed), what you wrote in them can be lost, even after an app
said it was saved (`fsync`). A normal `fev stop`, a restart of fev, or your Mac going to
sleep loses nothing: fev finishes what you wrote first.

For files that must not be lost, such as a database, keep them in a volume or machine and
use them from inside it, or keep a copy on the Mac.

## When fev stops or the VM fails

Nothing on your Mac waits for a VM that is gone. When fev stops, `~/Fevom` is unmounted
first, so what you wrote reaches the VM. If the VM crashes, or stops answering, fev unmounts
`~/Fevom` at once (within 5 seconds), and an app that had a file open there gets an error
for it instead of hanging. `fev status` says what happened:

```sh
fev status
# view: /Users/you/Fevom mounted
```

Files you have open in a container's, volume's or machine's folder while your Mac sleeps
can give an error when it wakes, if the Mac woke briefly on its own while closed (fev keeps
the VM paused then): open them again.

## Hiding it

To keep `~/Fevom` off Finder's sidebar and the desktop, with the folder still in place:

```sh
fev config set files.view_hidden true    # hidden; at once, files open and all
fev config set files.view_hidden false   # shown again
```

## Finder's own files

Finder and other Mac apps like to leave `.DS_Store` and `._` files in folders. fev refuses
them in `~/Fevom`, so none reach your Linux files. Copying a file with
extended attributes (tags, Finder comments) therefore warns that the attributes could not be
copied; the file itself is copied. Use `cp` or Finder rather than `ditto` to copy into
`~/Fevom`: `ditto` stops at its first such file.
