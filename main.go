package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/google/go-github/v57/github"
	"github.com/joho/godotenv"
	"github.com/shurcooL/githubv4"
	"golang.org/x/oauth2"
)

type CustomDate struct {
	time.Time
}

type ProjectV2ItemFieldValue struct {
	Field struct {
		ID   string
		Name string
	} `graphql:"field"`
	// Type information
	TypeName string `graphql:"__typename"`
	// These are for the different field types
	DateValue   *CustomDate `graphql:"... on ProjectV2ItemFieldDateValue"`
	NumberValue *float64    `graphql:"... on ProjectV2ItemFieldNumberValue"`
}

type ProjectV2Item struct {
	ID      string
	Project struct {
		ID     string
		Number int
		Title  string
	}
	FieldValues struct {
		Nodes []ProjectV2ItemFieldValue
	} `graphql:"fieldValues(first: 50)"`
}

// Config App configuration
type Config struct {
	Owner              string
	Repo               string
	Token              string
	IssueNumber        int
	IssueLabels        []string
	IssueBody          string
	ProjectCardURL     string
	FeatureIssueNumber int
	MilestoneNumber    int
	EventName          string
}

// FieldIDs Field IDs for a project
type FieldIDs struct {
	StartDateFieldID  string
	TargetDateFieldID string
	DurationFieldID   string
}

type FeatureIssue struct {
	Number      int
	Title       string
	Body        string
	Labels      []string
	Milestone   int
	TaskIDs     []int
	ProjectData []ProjectV2Item
	StartDate   *time.Time
	TargetDate  *time.Time
	Duration    *float64
}

// TaskIssue represents a task issue with all its data
type TaskIssue struct {
	Number      int
	Title       string
	Body        string
	FeatureID   int
	ProjectData []ProjectV2Item
	StartDate   *time.Time
	TargetDate  *time.Time
	Duration    *float64
}

// PlannedUpdate represents a planned update that will be executed in bulk
type PlannedUpdate struct {
	Type       string // "feature", "task", "milestone"
	ItemNumber int
	ProjectID  string
	ItemID     string
	FieldID    string
	FieldName  string
	OldValue   interface{}
	NewValue   interface{}
	HasChanged bool
}

// DateManagerService - modify to include caches
type DateManagerService struct {
	ctx           context.Context
	config        *Config
	restClient    *github.Client
	graphqlClient *githubv4.Client

	// Caches
	featureCache      map[int]*FeatureIssue
	taskCache         map[int]*TaskIssue
	milestoneCache    map[int]*github.Milestone
	projectFieldCache map[string]FieldIDs

	// Track changes to apply in bulk
	plannedUpdates []PlannedUpdate
}

// LoadAllData loads all needed data with minimal API calls
func (s *DateManagerService) LoadAllData(featureNumbers []int) error {
	// Initialize caches
	s.featureCache = make(map[int]*FeatureIssue)
	s.taskCache = make(map[int]*TaskIssue)
	s.milestoneCache = make(map[int]*github.Milestone)
	s.projectFieldCache = make(map[string]FieldIDs)
	s.plannedUpdates = []PlannedUpdate{}

	// 1. Load all features in bulk
	if len(featureNumbers) == 0 {
		// If no specific features provided, load all features from repo
		err := s.loadAllFeatures()
		if err != nil {
			return fmt.Errorf("failed to load features: %v", err)
		}
	} else {
		// Load specific features
		err := s.loadSpecificFeatures(featureNumbers)
		if err != nil {
			return fmt.Errorf("failed to load specific features: %v", err)
		}
	}

	// 2. Load all tasks
	err := s.loadAllTasks()
	if err != nil {
		return fmt.Errorf("failed to load tasks: %v", err)
	}

	// 3. Load all milestones referenced by features or tasks
	milestoneIDs := s.getAllMilestoneIDs()
	if len(milestoneIDs) > 0 {
		err := s.loadAllMilestones(milestoneIDs)
		if err != nil {
			return fmt.Errorf("failed to load milestones: %v", err)
		}
	}

	// 4. Load all project data for features and tasks
	err = s.loadAllProjectData()
	if err != nil {
		return fmt.Errorf("failed to load project data: %v", err)
	}

	fmt.Printf("Data loaded: %d features, %d tasks, %d milestones\n",
		len(s.featureCache), len(s.taskCache), len(s.milestoneCache))

	return nil
}

// loadAllFeatures loads all features from the repository in one query
func (s *DateManagerService) loadAllFeatures() error {
	fmt.Println("Loading all features from repository...")

	// Query for all issues with type:feature
	query := fmt.Sprintf("repo:%s/%s is:issue state:open type:feature",
		s.config.Owner, s.config.Repo)

	searchOptions := &github.SearchOptions{
		ListOptions: github.ListOptions{PerPage: 100},
	}

	var allFeatures []*github.Issue

	for {
		results, resp, err := s.restClient.Search.Issues(s.ctx, query, searchOptions)
		if err != nil {
			return err
		}

		for _, issue := range results.Issues {
			allFeatures = append(allFeatures, issue)
		}

		if resp.NextPage == 0 {
			break
		}
		searchOptions.Page = resp.NextPage
	}

	// Convert to our cache format
	for _, issue := range allFeatures {
		feature := &FeatureIssue{
			Number:  issue.GetNumber(),
			Title:   issue.GetTitle(),
			Body:    issue.GetBody(),
			TaskIDs: []int{},
		}

		// Extract labels
		for _, label := range issue.Labels {
			feature.Labels = append(feature.Labels, *label.Name)
		}

		// Get milestone if present
		if issue.Milestone != nil {
			feature.Milestone = issue.Milestone.GetNumber()
		}

		s.featureCache[feature.Number] = feature
	}

	fmt.Printf("Loaded %d features\n", len(s.featureCache))
	return nil
}

// loadSpecificFeatures loads specific feature issues by numbers
func (s *DateManagerService) loadSpecificFeatures(featureNumbers []int) error {
	if len(featureNumbers) == 0 {
		return nil
	}

	fmt.Printf("Loading %d specific features...\n", len(featureNumbers))

	// Use GraphQL to load issues in bulk
	var query struct {
		Repository struct {
			Issues struct {
				Nodes []struct {
					Number int
					Title  string
					Body   string
					Labels struct {
						Nodes []struct {
							Name string
						}
					} `graphql:"labels(first: 10)"`
					Milestone struct {
						Number int
					}
				}
			} `graphql:"issues(first: 100, numbers: $numbers)"`
		} `graphql:"repository(owner: $owner, name: $repo)"`
	}

	// Convert int slice to githubv4.Int slice
	issueNumbers := make([]githubv4.Int, len(featureNumbers))
	for i, num := range featureNumbers {
		issueNumbers[i] = githubv4.Int(num)
	}

	variables := map[string]interface{}{
		"owner":   githubv4.String(s.config.Owner),
		"repo":    githubv4.String(s.config.Repo),
		"numbers": issueNumbers,
	}

	err := s.graphqlClient.Query(s.ctx, &query, variables)
	if err != nil {
		return err
	}

	// Convert to our cache format
	for _, issue := range query.Repository.Issues.Nodes {
		feature := &FeatureIssue{
			Number:  issue.Number,
			Title:   issue.Title,
			Body:    issue.Body,
			TaskIDs: []int{},
		}

		// Extract labels
		for _, label := range issue.Labels.Nodes {
			feature.Labels = append(feature.Labels, label.Name)
		}

		// Get milestone if present
		if issue.Milestone.Number > 0 {
			feature.Milestone = issue.Milestone.Number
		}

		s.featureCache[feature.Number] = feature
	}

	fmt.Printf("Loaded %d specific features\n", len(s.featureCache))
	return nil
}

// extractAllTaskIDs extracts task IDs from all features in the cache
func (s *DateManagerService) extractAllTaskIDs() []int {
	taskIDMap := make(map[int]bool)
	taskListRegex := regexp.MustCompile(`-\s*\[\s*[x ]?\s*\]\s*#(\d+)`)
	taskRefRegex := regexp.MustCompile(`#(\d+)`)

	for _, feature := range s.featureCache {
		// Look for task list items with issue references in the body
		if feature.Body != "" {
			matches := taskListRegex.FindAllStringSubmatch(feature.Body, -1)
			for _, match := range matches {
				if len(match) > 1 {
					taskNumber, _ := strconv.Atoi(match[1])
					taskIDMap[taskNumber] = true
					feature.TaskIDs = append(feature.TaskIDs, taskNumber)
				}
			}

			// Also look for direct references that might be tasks
			matches = taskRefRegex.FindAllStringSubmatch(feature.Body, -1)
			for _, match := range matches {
				if len(match) > 1 {
					taskNumber, _ := strconv.Atoi(match[1])
					if !taskIDMap[taskNumber] {
						taskIDMap[taskNumber] = true
						feature.TaskIDs = append(feature.TaskIDs, taskNumber)
					}
				}
			}
		}
	}

	// Convert map to slice
	var taskIDs []int
	for id := range taskIDMap {
		taskIDs = append(taskIDs, id)
	}

	fmt.Printf("Extracted %d potential task IDs from features\n", len(taskIDs))
	return taskIDs
}

// loadAllTasks loads all tasks in bulk
func (s *DateManagerService) loadAllTasks() error {
	fmt.Println("Loading all tasks from repository...")

	// Query for all issues with type:task
	query := fmt.Sprintf("repo:%s/%s is:issue state:open type:task",
		s.config.Owner, s.config.Repo)

	searchOptions := &github.SearchOptions{
		ListOptions: github.ListOptions{PerPage: 100},
	}

	var allTasks []*github.Issue

	for {
		results, resp, err := s.restClient.Search.Issues(s.ctx, query, searchOptions)
		if err != nil {
			return err
		}

		for _, issue := range results.Issues {
			allTasks = append(allTasks, issue)
		}

		if resp.NextPage == 0 {
			break
		}
		searchOptions.Page = resp.NextPage
	}

	// Convert to our cache format
	for _, issue := range allTasks {
		task := &TaskIssue{
			Number:    issue.GetNumber(),
			Title:     issue.GetTitle(),
			Body:      issue.GetBody(),
			FeatureID: -1, // Will set this when analyzing
		}

		// Find parent feature
		parentFeatureID := s.findParentFeatureFromBody(issue.GetBody())
		if parentFeatureID > 0 {
			task.FeatureID = parentFeatureID

			// Make sure this task is in the feature's list
			if feature, ok := s.featureCache[parentFeatureID]; ok {
				if !contains(feature.TaskIDs, task.Number) {
					feature.TaskIDs = append(feature.TaskIDs, task.Number)
				}
			}
		}

		s.taskCache[task.Number] = task
	}

	fmt.Printf("Loaded %d tasks\n", len(s.taskCache))
	return nil
}

// findParentFeatureFromBody extracts parent feature reference from issue body
func (s *DateManagerService) findParentFeatureFromBody(body string) int {
	if body == "" {
		return -1
	}

	featureRegex := regexp.MustCompile(`(?i)(?:part of|belongs to|child of|task of|related to|parent:|feature:) #(\d+)`)
	matches := featureRegex.FindStringSubmatch(body)

	if len(matches) > 1 {
		featureID, _ := strconv.Atoi(matches[1])
		// Verify it's in our feature cache
		if _, ok := s.featureCache[featureID]; ok {
			return featureID
		}
	}

	return -1
}

// getAllMilestoneIDs collects all milestone IDs referenced by features and tasks
func (s *DateManagerService) getAllMilestoneIDs() []int {
	milestoneMap := make(map[int]bool)

	// Add milestone from config if specified
	if s.config.MilestoneNumber > 0 {
		milestoneMap[s.config.MilestoneNumber] = true
	}

	// Add milestones from features
	for _, feature := range s.featureCache {
		if feature.Milestone > 0 {
			milestoneMap[feature.Milestone] = true
		}
	}

	// Convert map to slice
	var milestoneIDs []int
	for id := range milestoneMap {
		milestoneIDs = append(milestoneIDs, id)
	}

	return milestoneIDs
}

// loadAllMilestones loads all milestones in bulk
func (s *DateManagerService) loadAllMilestones(milestoneIDs []int) error {
	if len(milestoneIDs) == 0 {
		return nil
	}

	fmt.Printf("Loading %d milestones...\n", len(milestoneIDs))

	// Unfortunately GitHub API doesn't support fetching milestones by ID in bulk
	// We have to fetch them one by one
	for _, id := range milestoneIDs {
		milestone, _, err := s.restClient.Issues.GetMilestone(s.ctx, s.config.Owner, s.config.Repo, id)
		if err != nil {
			return err
		}
		s.milestoneCache[id] = milestone
	}

	return nil
}

// loadAllProjectData loads project data for all features and tasks
func (s *DateManagerService) loadAllProjectData() error {
	// Collect all issue numbers (features and tasks)
	var allIssueNumbers []int

	for id := range s.featureCache {
		allIssueNumbers = append(allIssueNumbers, id)
	}

	for id := range s.taskCache {
		allIssueNumbers = append(allIssueNumbers, id)
	}

	if len(allIssueNumbers) == 0 {
		return nil
	}

	fmt.Printf("Loading project data for %d issues...\n", len(allIssueNumbers))

	// GraphQL has limits on variable size, so we need to batch
	batchSize := 20
	for i := 0; i < len(allIssueNumbers); i += batchSize {
		end := i + batchSize
		if end > len(allIssueNumbers) {
			end = len(allIssueNumbers)
		}

		batchIDs := allIssueNumbers[i:end]
		if err := s.loadProjectDataBatch(batchIDs); err != nil {
			return err
		}
	}

	return nil
}

// UnmarshalJSON implements json.Unmarshaler for CustomDate
func (d *CustomDate) UnmarshalJSON(data []byte) error {
	s := strings.Trim(string(data), "\"")
	if s == "null" {
		d.Time = time.Time{}
		return nil
	}

	formats := []string{
		"2006-01-02T15:04:05Z",      // ISO format
		"2006-01-02T15:04:05-07:00", // ISO with timezone
		"2006-01-02",                // Just date
	}

	var err error
	for _, format := range formats {
		t, err := time.Parse(format, s)
		if err == nil {
			d.Time = t
			return nil
		}
	}

	return err
}

// loadProjectDataBatch loads project data for a batch of issues
func (s *DateManagerService) loadProjectDataBatch(issueNumbers []int) error {
	for _, issueNumber := range issueNumbers {
		var query struct {
			Repository struct {
				Issue struct {
					ProjectItems struct {
						Nodes []struct {
							ID      string `graphql:"id"`
							Project struct {
								ID     string `graphql:"id"`
								Number int    `graphql:"number"`
								Title  string `graphql:"title"`
							} `graphql:"project"`
							FieldValues struct {
								Nodes []struct {
									TypeName string `graphql:"__typename"`

									// Date field fragment
									DateField struct {
										Date  CustomDate `graphql:"date"`
										Field struct {
											// For the field, we need to use fragments for the common fields
											// since Field might be a union type too
											CommonField struct {
												ID   string `graphql:"id"`
												Name string `graphql:"name"`
											} `graphql:"... on ProjectV2FieldCommon"`
										} `graphql:"field"`
									} `graphql:"... on ProjectV2ItemFieldDateValue"`

									// Number field fragment
									NumberField struct {
										Number float64 `graphql:"number"`
										Field  struct {
											CommonField struct {
												ID   string `graphql:"id"`
												Name string `graphql:"name"`
											} `graphql:"... on ProjectV2FieldCommon"`
										} `graphql:"field"`
									} `graphql:"... on ProjectV2ItemFieldNumberValue"`
								} `graphql:"nodes"`
							} `graphql:"fieldValues(first: 50)"`
						} `graphql:"nodes"`
					} `graphql:"projectItems(first: 20)"`
				} `graphql:"issue(number: $issueNumber)"`
			} `graphql:"repository(owner: $owner, name: $repo)"`
		}

		variables := map[string]interface{}{
			"owner":       githubv4.String(s.config.Owner),
			"repo":        githubv4.String(s.config.Repo),
			"issueNumber": githubv4.Int(issueNumber),
		}

		err := s.graphqlClient.Query(s.ctx, &query, variables)
		if err != nil {
			return err
		}

		// Convert result to ProjectV2Item objects
		var projectItems []ProjectV2Item

		for _, node := range query.Repository.Issue.ProjectItems.Nodes {
			item := ProjectV2Item{
				ID: node.ID,
				Project: struct {
					ID     string
					Number int
					Title  string
				}{
					ID:     node.Project.ID,
					Number: node.Project.Number,
					Title:  node.Project.Title,
				},
			}

			var fieldValues []ProjectV2ItemFieldValue

			for _, fieldNode := range node.FieldValues.Nodes {
				var fieldValue ProjectV2ItemFieldValue
				fieldValue.TypeName = fieldNode.TypeName

				// Extract field information based on the type
				if fieldNode.TypeName == "ProjectV2ItemFieldDateValue" {
					// Get field info from the date fragment
					fieldValue.Field.ID = fieldNode.DateField.Field.CommonField.ID
					fieldValue.Field.Name = fieldNode.DateField.Field.CommonField.Name

					// Check if we have a date value before using it
					if fieldNode.DateField.Date.Time.IsZero() {
						fieldValue.DateValue = nil
					} else {
						dateCopy := fieldNode.DateField.Date
						fieldValue.DateValue = &dateCopy

						// Handle dates for caches - only if we have a valid date
						dateTime := fieldNode.DateField.Date.Time
						if fieldNode.DateField.Field.CommonField.Name == "Start Date" {
							if feature, ok := s.featureCache[issueNumber]; ok {
								feature.StartDate = &dateTime
							}
							if task, ok := s.taskCache[issueNumber]; ok {
								task.StartDate = &dateTime
							}
						} else if fieldNode.DateField.Field.CommonField.Name == "Target Date" {
							if feature, ok := s.featureCache[issueNumber]; ok {
								feature.TargetDate = &dateTime
							}
							if task, ok := s.taskCache[issueNumber]; ok {
								task.TargetDate = &dateTime
							}
						}
					}
				} else if fieldNode.TypeName == "ProjectV2ItemFieldNumberValue" {
					// Get field info from the number fragment
					fieldValue.Field.ID = fieldNode.NumberField.Field.CommonField.ID
					fieldValue.Field.Name = fieldNode.NumberField.Field.CommonField.Name

					// Copy the number value
					numberVal := fieldNode.NumberField.Number
					fieldValue.NumberValue = &numberVal

					// Handle duration for caches
					if fieldNode.NumberField.Field.CommonField.Name == "Duration (days)" {
						value := fieldNode.NumberField.Number
						if feature, ok := s.featureCache[issueNumber]; ok {
							feature.Duration = &value
						}
						if task, ok := s.taskCache[issueNumber]; ok {
							task.Duration = &value
						}
					}
				}

				fieldValues = append(fieldValues, fieldValue)
			}

			item.FieldValues.Nodes = fieldValues
			projectItems = append(projectItems, item)

			// Rest of the code remains the same
			if _, ok := s.projectFieldCache[node.Project.ID]; !ok {
				fieldIDs, err := s.getProjectFieldIds(node.Project.ID)
				if err != nil {
					fmt.Printf("Warning: Failed to get field IDs for project %s: %v\n", node.Project.ID, err)
				} else {
					s.projectFieldCache[node.Project.ID] = fieldIDs
				}
			}
		}

		// Store project items in appropriate cache
		if feature, ok := s.featureCache[issueNumber]; ok {
			feature.ProjectData = projectItems
		}
		if task, ok := s.taskCache[issueNumber]; ok {
			task.ProjectData = projectItems
		}
	}

	return nil
}

// Run executes the date update process with optimized API calls
func (s *DateManagerService) Run() error {
	fmt.Printf("Starting date update process for %s/%s\n", s.config.Owner, s.config.Repo)

	featureIssueNumbers := []int{}

	// Determine which features to process based on the context
	if s.config.FeatureIssueNumber > 0 {
		featureIssueNumbers = append(featureIssueNumbers, s.config.FeatureIssueNumber)
	} else if s.config.IssueNumber > 0 {
		// Check if it's a feature
		isFeature := false
		for _, label := range s.config.IssueLabels {
			if strings.ToLower(label) == "feature" {
				isFeature = true
				break
			}
		}

		if isFeature {
			featureIssueNumbers = append(featureIssueNumbers, s.config.IssueNumber)
		}
		// We'll find parent features during data loading
	} else if s.config.ProjectCardURL != "" && strings.Contains(s.config.ProjectCardURL, "/issues/") {
		parts := strings.Split(s.config.ProjectCardURL, "/issues/")
		if len(parts) > 1 {
			if cardIssueNumber, err := strconv.Atoi(parts[1]); err == nil {
				// We'll check if this is a feature during data loading
				featureIssueNumbers = append(featureIssueNumbers, cardIssueNumber)
			}
		}
	}

	// Load all data in bulk
	err := s.LoadAllData(featureIssueNumbers)
	if err != nil {
		return fmt.Errorf("failed to load data: %v", err)
	}

	// Process all features and plan updates
	processedFeatures := 0
	for featureID, feature := range s.featureCache {
		if err := s.planFeatureUpdates(featureID); err != nil {
			log.Printf("Error planning updates for feature #%d: %v", featureID, err)
		} else {
			processedFeatures++
		}

		// Also plan to update milestone if feature has one
		if feature.Milestone > 0 {
			if err := s.planMilestoneUpdates(feature.Milestone); err != nil {
				log.Printf("Error planning updates for milestone #%d: %v", feature.Milestone, err)
			}
		}
	}

	// Handle explicitly requested milestone update
	if s.config.MilestoneNumber > 0 {
		if err := s.planMilestoneUpdates(s.config.MilestoneNumber); err != nil {
			log.Printf("Error planning updates for requested milestone #%d: %v", s.config.MilestoneNumber, err)
		}
	}

	// Present planned updates to user
	s.presentPlannedUpdates()

	// Apply the updates in bulk
	if len(s.plannedUpdates) > 0 {
		if err := s.applyPlannedUpdates(); err != nil {
			return fmt.Errorf("error applying updates: %v", err)
		}
	} else {
		fmt.Println("No updates needed - everything is up to date")
	}

	fmt.Println("Date update process complete")
	return nil
}

// planFeatureUpdates calculates and plans necessary updates for a feature and its tasks
func (s *DateManagerService) planFeatureUpdates(featureID int) error {
	feature, ok := s.featureCache[featureID]
	if !ok {
		return fmt.Errorf("feature #%d not found in cache", featureID)
	}

	fmt.Printf("\n--- Processing feature #%d: %s ---\n", featureID, feature.Title)

	// Find tasks with missing dates and plan their updates
	var tasksWithMissingDates []int

	// Track earliest start and latest target dates across all tasks
	var earliestStartDate time.Time
	var latestTargetDate time.Time

	// Process each task
	for _, taskID := range feature.TaskIDs {
		task, ok := s.taskCache[taskID]
		if !ok {
			continue // Task not found or not loaded
		}

		// Check if task has dates
		hasDates := task.StartDate != nil && task.TargetDate != nil

		if hasDates {
			// Update our tracking of earliest/latest dates
			if task.StartDate != nil {
				if earliestStartDate.IsZero() || task.StartDate.Before(earliestStartDate) {
					earliestStartDate = *task.StartDate
				}
			}

			if task.TargetDate != nil {
				if latestTargetDate.IsZero() || task.TargetDate.After(latestTargetDate) {
					latestTargetDate = *task.TargetDate
				}
			}
		} else {
			tasksWithMissingDates = append(tasksWithMissingDates, taskID)
		}
	}

	// If no tasks have dates, set default start date to today
	if earliestStartDate.IsZero() {
		earliestStartDate = getNextBusinessDay(time.Now())
		fmt.Printf("No start dates found in tasks. Using today (%s) as the default start date.\n",
			earliestStartDate.Format("2006-01-02"))
	}

	// Plan updates for tasks with missing dates
	if len(tasksWithMissingDates) > 0 {
		fmt.Printf("Found %d tasks with missing dates. Planning default dates...\n", len(tasksWithMissingDates))

		var currentDate time.Time
		if !latestTargetDate.IsZero() {
			currentDate = latestTargetDate.AddDate(0, 0, 1)
		} else {
			currentDate = earliestStartDate
		}

		for _, taskID := range tasksWithMissingDates {
			task, ok := s.taskCache[taskID]
			if !ok {
				continue
			}

			// Add one business day for each task
			currentDate = getNextBusinessDay(currentDate)
			taskStartDate := currentDate

			currentDate = currentDate.AddDate(0, 0, 1)
			taskTargetDate := getNextBusinessDay(currentDate)

			fmt.Printf("Planning default dates for task #%d: Start=%s, Target=%s\n",
				taskID, taskStartDate.Format("2006-01-02"), taskTargetDate.Format("2006-01-02"))

			// Plan updates for this task in all its projects
			for _, projectItem := range task.ProjectData {
				fieldIDs, ok := s.projectFieldCache[projectItem.Project.ID]
				if !ok {
					continue
				}

				// Plan start date update
				if fieldIDs.StartDateFieldID != "" {
					s.plannedUpdates = append(s.plannedUpdates, PlannedUpdate{
						Type:       "task",
						ItemNumber: taskID,
						ProjectID:  projectItem.Project.ID,
						ItemID:     projectItem.ID,
						FieldID:    fieldIDs.StartDateFieldID,
						FieldName:  "Start Date",
						OldValue:   task.StartDate,
						NewValue:   taskStartDate,
						HasChanged: true,
					})
				}

				// Plan target date update
				if fieldIDs.TargetDateFieldID != "" {
					s.plannedUpdates = append(s.plannedUpdates, PlannedUpdate{
						Type:       "task",
						ItemNumber: taskID,
						ProjectID:  projectItem.Project.ID,
						ItemID:     projectItem.ID,
						FieldID:    fieldIDs.TargetDateFieldID,
						FieldName:  "Target Date",
						OldValue:   task.TargetDate,
						NewValue:   taskTargetDate,
						HasChanged: true,
					})
				}

				// Plan duration update
				if fieldIDs.DurationFieldID != "" {
					duration := float64(taskTargetDate.Sub(taskStartDate).Hours()/24) + 1
					if duration < 1 {
						duration = 1
					}

					s.plannedUpdates = append(s.plannedUpdates, PlannedUpdate{
						Type:       "task",
						ItemNumber: taskID,
						ProjectID:  projectItem.Project.ID,
						ItemID:     projectItem.ID,
						FieldID:    fieldIDs.DurationFieldID,
						FieldName:  "Duration (days)",
						OldValue:   task.Duration,
						NewValue:   duration,
						HasChanged: true,
					})
				}
			}

			// Update our tracking of latest date
			if latestTargetDate.IsZero() || taskTargetDate.After(latestTargetDate) {
				latestTargetDate = taskTargetDate
			}

			// Cache the planned dates for milestone calculations
			startDateCopy := taskStartDate // Create copy to store pointer safely
			targetDateCopy := taskTargetDate
			task.StartDate = &startDateCopy
			task.TargetDate = &targetDateCopy
		}
	}

	// Now plan updates for the feature itself
	if len(feature.ProjectData) > 0 && !earliestStartDate.IsZero() && !latestTargetDate.IsZero() {
		for _, projectItem := range feature.ProjectData {
			fieldIDs, ok := s.projectFieldCache[projectItem.Project.ID]
			if !ok {
				continue
			}

			// Plan start date update
			if fieldIDs.StartDateFieldID != "" {
				shouldUpdate := feature.StartDate == nil ||
					!feature.StartDate.Equal(earliestStartDate)

				s.plannedUpdates = append(s.plannedUpdates, PlannedUpdate{
					Type:       "feature",
					ItemNumber: featureID,
					ProjectID:  projectItem.Project.ID,
					ItemID:     projectItem.ID,
					FieldID:    fieldIDs.StartDateFieldID,
					FieldName:  "Start Date",
					OldValue:   feature.StartDate,
					NewValue:   earliestStartDate,
					HasChanged: shouldUpdate,
				})
			}

			// Plan target date update
			if fieldIDs.TargetDateFieldID != "" {
				shouldUpdate := feature.TargetDate == nil ||
					!feature.TargetDate.Equal(latestTargetDate)

				s.plannedUpdates = append(s.plannedUpdates, PlannedUpdate{
					Type:       "feature",
					ItemNumber: featureID,
					ProjectID:  projectItem.Project.ID,
					ItemID:     projectItem.ID,
					FieldID:    fieldIDs.TargetDateFieldID,
					FieldName:  "Target Date",
					OldValue:   feature.TargetDate,
					NewValue:   latestTargetDate,
					HasChanged: shouldUpdate,
				})
			}

			// Plan duration update
			if fieldIDs.DurationFieldID != "" {
				// Calculate duration in days (including the target date)
				duration := float64(latestTargetDate.Sub(earliestStartDate).Hours()/24) + 1
				if duration < 1 {
					duration = 1 // Minimum duration of 1 day
				}

				shouldUpdate := feature.Duration == nil || *feature.Duration != duration

				s.plannedUpdates = append(s.plannedUpdates, PlannedUpdate{
					Type:       "feature",
					ItemNumber: featureID,
					ProjectID:  projectItem.Project.ID,
					ItemID:     projectItem.ID,
					FieldID:    fieldIDs.DurationFieldID,
					FieldName:  "Duration (days)",
					OldValue:   feature.Duration,
					NewValue:   duration,
					HasChanged: shouldUpdate,
				})
			}
		}
	}

	return nil
}

// planMilestoneUpdates plans updates for a milestone based on its issues
func (s *DateManagerService) planMilestoneUpdates(milestoneNumber int) error {
	milestone, ok := s.milestoneCache[milestoneNumber]
	if !ok {
		return fmt.Errorf("milestone #%d not found in cache", milestoneNumber)
	}

	fmt.Printf("\n--- Processing milestone #%d: %s ---\n", milestoneNumber, milestone.GetTitle())

	// Find the latest target date from all issues in this milestone
	var latestTargetDate time.Time

	// Check features
	for _, feature := range s.featureCache {
		if feature.Milestone == milestoneNumber && feature.TargetDate != nil {
			if latestTargetDate.IsZero() || feature.TargetDate.After(latestTargetDate) {
				latestTargetDate = *feature.TargetDate
			}
		}
	}

	// Check tasks directly in this milestone
	for _, task := range s.taskCache {
		// We don't store milestone for tasks, but we know their feature
		if task.FeatureID > 0 {
			if feature, ok := s.featureCache[task.FeatureID]; ok && feature.Milestone == milestoneNumber {
				if task.TargetDate != nil {
					if latestTargetDate.IsZero() || task.TargetDate.After(latestTargetDate) {
						latestTargetDate = *task.TargetDate
					}
				}
			}
		}
	}

	if !latestTargetDate.IsZero() {
		// Check if the milestone due date needs updating
		currentDueDate := milestone.DueOn
		if currentDueDate == nil || !currentDueDate.Time.Equal(latestTargetDate) {
			s.plannedUpdates = append(s.plannedUpdates, PlannedUpdate{
				Type:       "milestone",
				ItemNumber: milestoneNumber,
				FieldName:  "Due Date",
				OldValue:   currentDueDate,
				NewValue:   latestTargetDate,
				HasChanged: true,
			})
		}
	} else {
		fmt.Printf("No target dates found for issues in milestone #%d\n", milestoneNumber)
	}

	return nil
}

// presentPlannedUpdates shows all planned updates to the user
func (s *DateManagerService) presentPlannedUpdates() {
	if len(s.plannedUpdates) == 0 {
		fmt.Println("\nNo updates needed - all dates are already correct")
		return
	}

	fmt.Printf("\n=== Planned Updates (%d total) ===\n", len(s.plannedUpdates))

	// Group by issue
	featureUpdates := make(map[int][]PlannedUpdate)
	taskUpdates := make(map[int][]PlannedUpdate)
	milestoneUpdates := make(map[int][]PlannedUpdate)

	for _, update := range s.plannedUpdates {
		if !update.HasChanged {
			continue // Skip updates that don't change anything
		}

		switch update.Type {
		case "feature":
			featureUpdates[update.ItemNumber] = append(featureUpdates[update.ItemNumber], update)
		case "task":
			taskUpdates[update.ItemNumber] = append(taskUpdates[update.ItemNumber], update)
		case "milestone":
			milestoneUpdates[update.ItemNumber] = append(milestoneUpdates[update.ItemNumber], update)
		}
	}

	// Print feature updates
	if len(featureUpdates) > 0 {
		fmt.Println("\n-- Feature Updates --")
		for featureID, updates := range featureUpdates {
			feature, ok := s.featureCache[featureID]
			if !ok {
				continue
			}

			fmt.Printf("Feature #%d: %s\n", featureID, feature.Title)
			for _, update := range updates {
				oldVal := formatUpdateValue(update.OldValue)
				newVal := formatUpdateValue(update.NewValue)
				fmt.Printf("  • %s: %s → %s\n", update.FieldName, oldVal, newVal)
			}
		}
	}

	// Print task updates
	if len(taskUpdates) > 0 {
		fmt.Println("\n-- Task Updates --")
		for taskID, updates := range taskUpdates {
			task, ok := s.taskCache[taskID]
			if !ok {
				continue
			}

			var featureTitle string
			if task.FeatureID > 0 {
				if feature, ok := s.featureCache[task.FeatureID]; ok {
					featureTitle = fmt.Sprintf(" (part of Feature #%d: %s)", task.FeatureID, feature.Title)
				}
			}

			fmt.Printf("Task #%d: %s%s\n", taskID, task.Title, featureTitle)
			for _, update := range updates {
				oldVal := formatUpdateValue(update.OldValue)
				newVal := formatUpdateValue(update.NewValue)
				fmt.Printf("  • %s: %s → %s\n", update.FieldName, oldVal, newVal)
			}
		}
	}

	// Print milestone updates
	if len(milestoneUpdates) > 0 {
		fmt.Println("\n-- Milestone Updates --")
		for milestoneID, updates := range milestoneUpdates {
			milestone, ok := s.milestoneCache[milestoneID]
			if !ok {
				continue
			}

			fmt.Printf("Milestone #%d: %s\n", milestoneID, milestone.GetTitle())
			for _, update := range updates {
				oldVal := formatUpdateValue(update.OldValue)
				newVal := formatUpdateValue(update.NewValue)
				fmt.Printf("  • %s: %s → %s\n", update.FieldName, oldVal, newVal)
			}
		}
	}

	fmt.Println("\nWould you like to apply these updates? (Y/n)")
	// In an automated environment, we'll just proceed
	fmt.Println("Running in automation mode - applying updates automatically")
}

// formatUpdateValue formats a value for display
func formatUpdateValue(value interface{}) string {
	if value == nil {
		return "not set"
	}

	switch v := value.(type) {
	case *time.Time:
		if v == nil {
			return "not set"
		}
		return v.Format("2006-01-02")
	case time.Time:
		return v.Format("2006-01-02")
	case *float64:
		if v == nil {
			return "not set"
		}
		return fmt.Sprintf("%.0f", *v)
	case float64:
		return fmt.Sprintf("%.0f", v)
	case *github.Timestamp:
		if v == nil {
			return "not set"
		}
		return v.Format("2006-01-02")
	default:
		return fmt.Sprintf("%v", value)
	}
}

// applyPlannedUpdates applies all planned updates in bulk where possible
func (s *DateManagerService) applyPlannedUpdates() error {
	changedUpdates := 0
	for _, update := range s.plannedUpdates {
		if !update.HasChanged {
			continue
		}
		changedUpdates++
	}

	fmt.Printf("\nApplying %d updates...\n", changedUpdates)

	// Apply project field updates
	projectUpdates := 0
	for _, update := range s.plannedUpdates {
		if !update.HasChanged {
			continue
		}

		if update.Type == "feature" || update.Type == "task" {
			// Update project fields
			var err error

			switch update.FieldName {
			case "Start Date", "Target Date":
				date, ok := update.NewValue.(time.Time)
				if !ok {
					return fmt.Errorf("invalid date value for %s update", update.Type)
				}
				err = s.updateProjectDateField(update.ProjectID, update.ItemID, update.FieldID, date)
			case "Duration (days)":
				duration, ok := update.NewValue.(float64)
				if !ok {
					return fmt.Errorf("invalid duration value for %s update", update.Type)
				}
				err = s.updateProjectDurationField(update.ProjectID, update.ItemID, update.FieldID, githubv4.Float(duration))
			}

			if err != nil {
				return fmt.Errorf("failed to update %s #%d %s: %v",
					update.Type, update.ItemNumber, update.FieldName, err)
			}

			projectUpdates++
		} else if update.Type == "milestone" {
			// Update milestone due date
			dueDate, ok := update.NewValue.(time.Time)
			if !ok {
				return fmt.Errorf("invalid due date value for milestone update")
			}

			milestone := &github.Milestone{
				DueOn: &github.Timestamp{Time: dueDate},
			}

			_, _, err := s.restClient.Issues.EditMilestone(
				s.ctx, s.config.Owner, s.config.Repo, update.ItemNumber, milestone)

			if err != nil {
				return fmt.Errorf("failed to update milestone #%d due date: %v",
					update.ItemNumber, err)
			}
		}
	}

	fmt.Printf("Successfully applied %d updates\n", changedUpdates)
	return nil
}

// getProjectFieldIds retrieves field IDs for a project
func (s *DateManagerService) getProjectFieldIds(projectId string) (FieldIDs, error) {
	var query struct {
		Node struct {
			Project struct {
				Fields struct {
					Nodes []struct {
						// Add __typename to help with debugging
						TypeName string `graphql:"__typename"`

						// Use a fragment to access fields on common interface
						CommonField struct {
							ID   string `graphql:"id"`
							Name string `graphql:"name"`
						} `graphql:"... on ProjectV2FieldCommon"`
					} `graphql:"nodes"`
				} `graphql:"fields(first: 50)"`
			} `graphql:"... on ProjectV2"`
		} `graphql:"node(id: $id)"`
	}

	variables := map[string]interface{}{
		"id": githubv4.ID(projectId),
	}

	err := s.graphqlClient.Query(s.ctx, &query, variables)
	if err != nil {
		return FieldIDs{}, err
	}

	fieldIds := FieldIDs{}
	for _, field := range query.Node.Project.Fields.Nodes {
		// We need to use the CommonField fragment to access ID and Name
		if field.CommonField.ID != "" { // Check if this field has common fields
			switch field.CommonField.Name {
			case "Start Date":
				fieldIds.StartDateFieldID = field.CommonField.ID
			case "Target Date":
				fieldIds.TargetDateFieldID = field.CommonField.ID
			case "Duration (days)":
				fieldIds.DurationFieldID = field.CommonField.ID
			}
		}
	}

	return fieldIds, nil
}

// updateProjectDateField updates a date field in a project
func (s *DateManagerService) updateProjectDateField(projectId string, itemId string, fieldId string, date time.Time) error {
	var mutation struct {
		UpdateProjectV2ItemFieldValue struct {
			ProjectV2Item struct {
				ID string
			}
		} `graphql:"updateProjectV2ItemFieldValue(input: $input)"`
	}

	input := githubv4.UpdateProjectV2ItemFieldValueInput{
		ProjectID: githubv4.ID(projectId),
		ItemID:    githubv4.ID(itemId),
		FieldID:   githubv4.ID(fieldId),
		Value: githubv4.ProjectV2FieldValue{
			Date: &githubv4.Date{Time: date},
		},
	}

	return s.graphqlClient.Mutate(s.ctx, &mutation, input, nil)
}

// updateProjectDurationField updates a number field in a project
func (s *DateManagerService) updateProjectDurationField(projectId string, itemId string, fieldId string, duration githubv4.Float) error {
	var mutation struct {
		UpdateProjectV2ItemFieldValue struct {
			ProjectV2Item struct {
				ID string
			}
		} `graphql:"updateProjectV2ItemFieldValue(input: $input)"`
	}

	input := githubv4.UpdateProjectV2ItemFieldValueInput{
		ProjectID: githubv4.ID(projectId),
		ItemID:    githubv4.ID(itemId),
		FieldID:   githubv4.ID(fieldId),
		Value: githubv4.ProjectV2FieldValue{
			Number: &duration,
		},
	}

	return s.graphqlClient.Mutate(s.ctx, &mutation, input, nil)
}

// getNextBusinessDay returns the next business day (skipping weekends)
func getNextBusinessDay(date time.Time) time.Time {
	result := date
	switch result.Weekday() {
	case time.Saturday:
		result = result.AddDate(0, 0, 2) // If Saturday, make it Monday
	case time.Sunday:
		result = result.AddDate(0, 0, 1) // If Sunday, make it Monday
	}
	return result
}

// Helper function to check if an int slice contains a value
func contains(slice []int, val int) bool {
	for _, item := range slice {
		if item == val {
			return true
		}
	}
	return false
}

// loadConfig loads application configuration from environment variables
func loadConfig() (*Config, error) {
	err := godotenv.Load()
	if err != nil {
		log.Println("No .env file found, using environment variables")
	}

	config := &Config{
		Owner:          os.Getenv("OWNER"),
		Repo:           os.Getenv("REPO"),
		Token:          os.Getenv("GITHUB_TOKEN"),
		IssueBody:      os.Getenv("ISSUE_BODY"),
		ProjectCardURL: os.Getenv("PROJECT_CARD_URL"),
		EventName:      os.Getenv("GITHUB_EVENT_NAME"),
	}

	if issueNum := os.Getenv("ISSUE_NUMBER"); issueNum != "" {
		config.IssueNumber, _ = strconv.Atoi(issueNum)
	}

	if featureNum := os.Getenv("FEATURE_ISSUE_NUMBER"); featureNum != "" {
		config.FeatureIssueNumber, _ = strconv.Atoi(featureNum)
	}

	if milestoneNum := os.Getenv("MILESTONE_NUMBER"); milestoneNum != "" {
		config.MilestoneNumber, _ = strconv.Atoi(milestoneNum)
	}

	if labelJSON := os.Getenv("ISSUE_LABELS"); labelJSON != "" {
		re := regexp.MustCompile(`"name":"([^"]+)"`)
		matches := re.FindAllStringSubmatch(labelJSON, -1)
		for _, match := range matches {
			if len(match) > 1 {
				config.IssueLabels = append(config.IssueLabels, match[1])
			}
		}
	}

	return config, nil
}

func main() {
	config, err := loadConfig()
	if err != nil {
		log.Fatalf("Failed to load configuration: %v", err)
	}

	ctx := context.Background()
	ts := oauth2.StaticTokenSource(
		&oauth2.Token{AccessToken: config.Token},
	)
	tc := oauth2.NewClient(ctx, ts)

	restClient := github.NewClient(tc)
	graphqlClient := githubv4.NewClient(tc)

	service := &DateManagerService{
		ctx:           ctx,
		config:        config,
		restClient:    restClient,
		graphqlClient: graphqlClient,
	}

	if err := service.Run(); err != nil {
		log.Fatalf("Error: %v", err)
	}
}
