# Security and publication boundaries

This is a public repository. Publish reusable implementation, fictional examples
and fixture data only. Use reserved example domains and loopback addresses.

Keep real hosts/domains, SSH ports, usernames, home/workspace paths, certificates,
keys, account databases, clipboard content, container inspect dumps, image/backup
inventories and operational evidence outside the checkout. Never add them to PR
bodies, issues, comments, Actions logs, screenshots or downloadable artifacts.

`.gitignore` and `.dockerignore` reduce accidental inclusion; they do not remove
tracked files, clean history or inspect text. Run `node scripts/check-public-docs.mjs`
and review `git diff --cached` and artifact inputs before pushing. The checker
catches known classes of publication mistakes; it cannot prove arbitrary text is
free of all sensitive information. Add new legitimate external hosts deliberately.

For an exposure, stop publishing affected data, preserve evidence privately,
rotate exposed credentials if present, clean every affected branch/tag and
force-push with a verified lease. Remove affected PR text and Actions logs/artifacts.
Other clones must be cleaned or re-cloned; merging old history can reintroduce it.

Force-pushing does not erase GitHub cached SHA views or read-only PR references.
Follow [GitHub's removal procedure](https://docs.github.com/en/authentication/keeping-your-account-and-data-secure/removing-sensitive-data-from-a-repository)
and request platform assistance where applicable. Keep the evidence and removal
request private; do not repeat the exposed values in a public cleanup issue.
GitHub determines whether platform removal is available for the reported data.

For a vulnerability report, do not open a public issue containing secrets or
private deployment details. Use the repository's private security reporting
channel if enabled, or arrange a private channel with the maintainer first.
