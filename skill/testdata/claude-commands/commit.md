---
allowed-tools: Bash(git add:*), Bash(git status:*), Bash(git commit:*)
argument-hint: [message]
description: Create a git commit
model: claude-3-5-haiku-20241022
---

## Context

- Current git status: !`git status`

## Your task

Create a single git commit. Message hint: $ARGUMENTS
