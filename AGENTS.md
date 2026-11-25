# Agent Instructions

## No code comments

Comments drift from the code they describe, and agents trust them over the code.

- Do not add comments to code: no doc comments, line comments, or block comments.
- Make the code explain itself through names, small functions, and tests.
- Keep only comments that change behavior: `//go:build`, `//go:embed`, and `//nolint`.
- When you edit code that already has comments, do not trust them. Read the code instead.
