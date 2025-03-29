# GitHub Project Date Manager Action

A GitHub Action that automatically manages dates for GitHub Projects, keeping features, tasks, and milestones in sync.

## What This Action Does

- **Syncs Feature Dates with Tasks**: Sets feature start and target dates based on their subtasks
- **Handles Missing Dates**: Automatically assigns reasonable dates to tasks without dates
- **Updates Milestones**: Sets milestone due dates based on the latest target date of contained issues
- **Respects Business Days**: Ensures all dates fall on weekdays (skips weekends)
- **Finds Relationships**: Intelligently detects parent-child relationships between issues

## Usage

Add this action to your workflow file (e.g., `.github/workflows/update-dates.yml`):

```yaml
name: Update Project Dates

on:
  issues:
    types: [opened, edited, labeled, unlabeled, closed, reopened]
  project_card:
    types: [created, edited, moved]
  milestone:
    types: [created, edited, opened, closed, deleted]
  workflow_dispatch:
    inputs:
      feature_issue_number:
        description: 'Feature issue number to update'
        required: false
      milestone_number:
        description: 'Milestone number to update'
        required: false

jobs:
  update-dates:
    runs-on: ubuntu-latest
    steps:
      - name: Update Project Dates
        uses: nimling/github-date-manager-action@v1.0.0
        with:
          # Optional parameters (defaults should work for most cases)
          github-token: ${{ secrets.GITHUB_TOKEN }}
          # feature-issue-number: 123  # To update a specific feature
          # milestone-number: 5        # To update a specific milestone
```

## Requirements

Your GitHub Projects must have these fields:
- **Start Date** (Date type)
- **Target Date** (Date type)
- **Duration (days)** (Number type, optional)

## How It Works

### Feature & Task Relationship

The action recognizes parent-child relationships through:
- Issues labeled as "feature" containing task lists (e.g., `- [ ] #123`)
- Task issues that reference features (e.g., "Part of #456" in the body)
- Issues with the "task" label that reference features

### Date Calculation Logic

1. **For features**:
    - Start date = earliest start date among all tasks
    - Target date = latest target date among all tasks

2. **For tasks without dates**:
    - If some tasks have dates, new tasks are scheduled after the latest one
    - If no tasks have dates, scheduling starts from today
    - Each task gets a default 1-day duration
    - All dates are adjusted to fall on weekdays

3. **For milestones**:
    - Due date = latest target date of any issue in the milestone

## Inputs

| Input | Description | Required | Default |
|-------|-------------|----------|---------|
| `github-token` | GitHub token with repository access | Yes | `${{ github.token }}` |
| `owner` | Repository owner | No | Current repository owner |
| `repo` | Repository name | No | Current repository name |
| `feature-issue-number` | Specific feature issue to update | No | |
| `milestone-number` | Specific milestone to update | No | |

## License

MIT

## Contributing

Pull requests and suggestions welcome!