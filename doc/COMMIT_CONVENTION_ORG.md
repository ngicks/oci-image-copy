# Commit convention

Every commit message in this repository follows this document.  
A commit message is a subject line, optionally followed by a short body.

## Subject

The subject has no trailing period:

```
<emoji>(<scope>): brief description of change
```

- `<emoji>` — exactly one prefix emoji from the **Emoji prefixes** table.
- `(<scope>)` — what the change touches, chosen by the **Scope rules** table.
- Description — short, lowercase, imperative mood
  ("add", "fix", "remove" — not "added", "adds").

## Emoji prefixes

Pick the one emoji matching the primary intent of the change.

| prefix | meaning                                             |
| :----- | :-------------------------------------------------- |
| ✨     | new or change of features                           |
| 🐛     | bug fixes                                           |
| 🚀     | performance optimization                            |
| 🧹     | cleaning / refactor                                 |
| 🔥     | removing stuff                                      |
| 📦     | moving files / bump dep versions                    |
| ✅     | add / change tests                                  |
| 📝     | add / change documents                              |
| 👷     | change of build / build constraint / CI/CD workflow |
| 🔖     | tag / release                                       |
| 💄     | UI and styles                                       |
| 🚧     | work in progress                                    |
| 🔊     | add / change logs                                   |

Notes on choosing:

- Skills, instructions, and other prompt files are documents — changes to
  them are 📝, even when they change agent behavior.
- Adding a new tool, hook, or skill directory from scratch is ✨.
- Only these emojis are allowed; they are chosen to render well on all
  platforms the author uses (neovim, lazygit, web browser, IDEs).
  Do not substitute other gitmoji.
- When a commit mixes intents, prefer splitting into one commit per intent.
  If splitting is not worth it, pick the emoji for the dominant intent —
  never stack multiple emojis on one subject line.

## Scope rules

The scope names the module, package, skill, or directory the change touches.

| situation                                   | scope                                     | example                                      |
| :------------------------------------------ | :---------------------------------------- | :------------------------------------------- |
| one module / package / skill                | its row in the **Scopes** table           | `✨(tool): add default targets`              |
| a few modules                               | comma-separated, no spaces                | `📝(cc-workers,nggoal): ask availability`    |
| cross-cutting, or scopes exceed ~16 chars   | omit the parentheses entirely             | `📝: all instructions non-conditional`       |
| no meaningful scope (repo root, misc files) | omit the parentheses entirely             | `🐛: fix typo`                               |
| not in the **Scopes** table                 | add a row first, in its own 📝 commit     | `📝(doc): add scope for new-skill`           |

- Use the name as it appears in the tree (directory or package name),
  not a description of it.
- Never write empty parentheses; either name a scope or omit them.
- Do not invent a scope on the spot. The **Scopes** table is the only
  list of valid scopes; nothing else in the repository defines them.

## Scopes

This table lists every scope this repository uses.  
It is project-specific: fill it when this document is created, and add a row
whenever a new module, package, skill, or directory appears.

A single scope uses the tree name as-is, even when it is long. The ~16
character limit in the scope rules applies to comma-joined scopes.

| scope     | covers                                                     |
| :-------- | :--------------------------------------------------------- |
| `doc`     | `doc/`, including this document                            |
| `<name>`  | one row per module / package / skill, named as in the tree |

Guidelines for rows:

- Prefer one row per directory that gets its own commits.
  Group small or rarely-touched directories under one scope instead.
- Remove a row when its directory is removed; keep the table matching the tree.

## Body

Most commits need no body — the subject alone is enough.  
When context matters (why, not what), add one after a blank line:

- Keep it short — around 2-3 lines.
- If the change is significant, going longer is fine.

## Examples

```
✨(tool): add default targets
🐛: fix typo
📝(go-edit-cobra): forbid non-inline field in *cobra.Command
🔥: remove fragile test
👷: bump LLM stuff
📦: bump golang.org/x/sys to v0.30.0
```
