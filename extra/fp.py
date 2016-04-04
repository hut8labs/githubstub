#!/usr/bin/env python

from optparse import OptionParser
import json
import os
from subprocess import check_call, PIPE, Popen
import sys
from textwrap import dedent


def main():
    usage = dedent("""\
                   %prog [options] </base/repo/dir>

                   Simulate, more or less, a git push event from a a github
                   webhook event.

                   Acomplish this by parsing a Github push event's json from
                   stdin and doing the following:

                   - find the repo of the same name under /base/repo/dir and cd
                     to it

                   - call git fetch in the repo

                   - calls post-receive in the repo's hooks dir with the "ref",
                     "before", and "after" as parsed from the json passed in on
                     stdin
                   """)
    parser = OptionParser()

    opts, args = parser.parse_args()

    if not args:
        parser.error("Base repo dir is required")

    base_repo_dir = args[0]

    payload = json.load(sys.stdin)
    repo_name = payload["repository"]["name"]
    ref = payload["ref"]
    before = payload["before"]
    after = payload["after"]

    _validate_post_receive_args(ref, before, after)

    repo_dir = os.path.join(base_repo_dir, repo_name)
    os.chdir(repo_dir)
    check_call(["git", "fetch"])
    pr = Popen(["hooks/post-receive"], stdin=PIPE, stderr=PIPE, stdout=PIPE)
    outd, errd = pr.communicate("%s %s %s\n" % (before, after, ref))
    print outd
    print >> sys.stderr, errd
    sys.exit(pr.returncode)


def _validate_post_receive_args(ref, before, after):
    if not ref:
        raise Exception("No ref found")

    if not before:
        raise Exception("No before found")

    if not after:
        raise Exception("No after found")


if __name__ == '__main__':
    main()




