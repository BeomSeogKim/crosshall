# Classifies `ps -o pid= -o etime= -o command=` lines for the release gate's
# full-run row (#391). Variables: work and out, the gate's directories.
#   gate:  the executable is under work or out, a binary this gate built, so
#          an earlier row started it and left it running;
#   test:  another test run: go test, a Go test binary, or ci-checks.sh;
#   names: the command line only names a gate path; who started it is unknown.
# command= joins argv with spaces, so an executable under a gate path that
# holds a space spans fields: match the command line's start instead.
{
  exe = $3
  n = split(exe, part, "/")
  base = part[n]
  command = $0
  sub(/^[ \t]*[^ \t]+[ \t]+[^ \t]+[ \t]+/, "", command)
}
index(command, work "/") == 1 || index(command, out "/") == 1 { print "gate: " $0; next }
(base == "go" && $4 == "test") || (base ~ /\.test$/ && $4 ~ /^-test\./) ||
  ((base == "bash" || base == "sh") && $4 ~ /(^|\/)ci-checks\.sh$/) { print "test: " $0; next }
index($0, work "/") || index($0, out "/") { print "names: " $0 }
