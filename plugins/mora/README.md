# Mora agent skills

Agent-side skills that pair with the Mora MCP server. Each skill is a plain-Markdown recipe (`skills/<name>/SKILL.md`) that teaches an agent to ground a live task — recommendations, planning, prep — in your vault: resolve the people involved, pull shared history and calendar context, *then* do the live web work.

The MCP server is the substrate; these are recipes for using it well. No skill adds code to Mora or sends your data anywhere — the web layer runs in your agent, the personal layer stays on your machine.

## Install (Claude Code)

```
/plugin marketplace add pyranthus-hq/mora
/plugin install mora@mora
```

Skills appear namespaced: `/mora:dining-concierge`. They also trigger automatically when a request matches a skill's description. Skills track the repo — updating the plugin pulls the latest.

Requires the Mora MCP server to be connected (see the [main README](../../README.md#install)).

## Other agents

Skills are plain Markdown with YAML frontmatter — portable to any harness that reads the format. Codex: copy `skills/<name>/` into `~/.agents/skills/`.

## Skills

| Skill | Use it for |
|---|---|
| `dining-concierge` | Restaurant/bar/outing recommendations grounded in who's coming, where you've actually been, and what's already on your calendar |

## Adding a skill

PR a `skills/<name>/SKILL.md`. Conventions:

- The frontmatter `description` states **when to use it** (triggers, symptoms) — not a workflow summary.
- Personal layer before web layer; cite retrieved memories, never invent preferences; state what the vault does **not** know.
- Examples in skill text use synthetic names and venues — vault data is personal, skill files are public.
