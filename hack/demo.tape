# Demo recording for the README. Run with `just demo`.
#
# Driven by hack/demo/serve.py, which replays a real capture from a kind cluster
# rather than a live daemon. A live daemon needs a cluster, and the kills it
# records differ every run in timing, PIDs and trajectory shape, so the GIF would
# change on every rebuild for reasons nobody chose. Nothing here is fabricated;
# it is replayed.
#
# Every command in the recording is really executed. An earlier draft typed a
# `kubectl get pod` for contrast and let it run against whatever cluster was
# configured, which put "Error from server (NotFound)" in the middle of the
# demo. If a command cannot honestly run here, it does not belong in the tape.

Output docs/assets/demo.gif

Set Shell bash
Set FontSize 14
Set Width 1500
Set Height 850
Set Padding 20
Set Theme "Catppuccin Mocha"
Set TypingSpeed 40ms

Hide
Type "export PATH=$PWD/bin:$PATH && clear"
Enter
Sleep 1s
Show

Type "# Kubernetes says OOMKilled and exit code 137, then stops."
Enter
Sleep 1800ms
Type "# It cannot say which process died, or what the climb looked like."
Enter
Sleep 2200ms

Type "oom-oracle inspect checkout-api-7f9d2c"
Enter
Sleep 7s

Type "clear"
Enter
Sleep 400ms

Type "# Or watch a node live."
Enter
Sleep 1600ms

Type "oom-oracle watch"
Enter
Sleep 3s

# Move down the list so the two panes are obviously linked.
Down
Sleep 2200ms
Down
Sleep 2200ms
Down
Sleep 2400ms
Up
Sleep 2000ms

Type "q"
Sleep 1500ms
