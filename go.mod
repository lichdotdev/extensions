module github.com/lichdotdev/extensions

go 1.27.0

require github.com/lichdotdev/lich/extension v0.2.0

// shortcut: local replace until the SDK tag with catalog.go ships, then require the tag
replace github.com/lichdotdev/lich/extension => /home/meopedevts/.local/share/lich/worktrees/e48f04f46a4a/jolly-pine/extension
