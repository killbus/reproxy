# Backend Development Guidelines

> Best practices for backend development in this project.

---

## Overview

This directory contains guidelines for backend development. Fill in each file with your project's specific conventions.

---

## Guidelines Index

| Guide | Description | Status |
|-------|-------------|--------|
| [Retry Proxy Contract](./retry-proxy-contract.md) | Query namespace, policy resolution, budget, response headers — executable protocol contract | Filled (09-04) |
| [Directory Structure](./directory-structure.md) | Module organization and file layout | To fill |
| [Database Guidelines](./database-guidelines.md) | ORM patterns, queries, migrations | N/A (no DB) |
| [Error Handling](./error-handling.md) | Commit point, race cleanup, SSRF, streaming semantics | Filled (09-04) |
| [Quality Guidelines](./quality-guidelines.md) | Test hardening, mutation scanning, forbidden patterns | Filled (09-04) |
| [Logging Guidelines](./logging-guidelines.md) | Structured logging, log levels | See observability section in quality-guidelines.md |

---

## How to Fill These Guidelines

For each guideline file:

1. Document your project's **actual conventions** (not ideals)
2. Include **code examples** from your codebase
3. List **forbidden patterns** and why
4. Add **common mistakes** your team has made

The goal is to help AI assistants and new team members understand how YOUR project works.

---

**Language**: All documentation should be written in **English**.
