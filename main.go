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

type ProjectV2ItemFieldValue struct {
	Field struct {
		ID   string
		Name string
	} `graphql:"field"`
	// Type information
	TypeName string `graphql:"__typename"`
	// These are for the different field types
	DateValue   *githubv4.Date `graphql:"... on ProjectV2ItemFieldDateValue"`
	NumberValue *float64       `graphql:"... on ProjectV2ItemFieldNumberValue"`
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

type ProjectV2Field struct {
	ID   string
	Name string
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

// TaskWithMissingDates Task with missing dates
type TaskWithMissingDates struct {
	TaskNumber   int
	ProjectItems []ProjectV2Item
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

// loadConfig Load environment variables from .env file
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

// Run executes the date update process
func (s *DateManagerService) Run() error {
	fmt.Printf("Starting date update process for %s/%s\n", s.config.Owner, s.config.Repo)

	// Initialize caches
	s.issueCache = make(map[int]*github.Issue)
	s.milestoneCache = make(map[int]*github.Milestone)
	s.projectDataCache = make(map[int][]ProjectV2Item)
	s.projectFieldCache = make(map[string]FieldIDs)

	featureIssueNumbers := []int{}

	// Case 1: Manual trigger with a specific feature
	if s.config.FeatureIssueNumber > 0 {
		fmt.Printf("Manual trigger for feature #%d\n", s.config.FeatureIssueNumber)
		featureIssueNumbers = append(featureIssueNumbers, s.config.FeatureIssueNumber)
	} else if s.config.IssueNumber > 0 {
		// Case 2: Working with a specific issue
		fmt.Printf("Working with issue #%d\n", s.config.IssueNumber)

		// Check if this is a feature
		isFeature := false
		for _, label := range s.config.IssueLabels {
			if strings.ToLower(label) == "feature" {
				isFeature = true
				break
			}
		}

		if isFeature {
			fmt.Printf("Issue #%d is a feature\n", s.config.IssueNumber)
			featureIssueNumbers = append(featureIssueNumbers, s.config.IssueNumber)
		} else {
			// If not a feature, find parent feature
			parentFeature, err := s.findParentFeature(s.config.IssueNumber)
			if err != nil {
				log.Printf("Error finding parent feature: %v", err)
			} else if parentFeature > 0 {
				fmt.Printf("Found parent feature #%d for issue #%d\n", parentFeature, s.config.IssueNumber)
				featureIssueNumbers = append(featureIssueNumbers, parentFeature)
			}
		}
	} else if s.config.ProjectCardURL != "" && strings.Contains(s.config.ProjectCardURL, "/issues/") {
		// Case 3: Project card context
		parts := strings.Split(s.config.ProjectCardURL, "/issues/")
		if len(parts) > 1 {
			if cardIssueNumber, err := strconv.Atoi(parts[1]); err == nil {
				fmt.Printf("Working with project card for issue #%d\n", cardIssueNumber)

				// Get the issue to check if it's a feature
				issue, _, err := s.restClient.Issues.Get(s.ctx, s.config.Owner, s.config.Repo, cardIssueNumber)
				if err != nil {
					log.Printf("Error getting issue #%d: %v", cardIssueNumber, err)
				} else {
					// Check if it's a feature
					isFeature := false
					for _, label := range issue.Labels {
						if strings.ToLower(*label.Name) == "feature" {
							isFeature = true
							break
						}
					}

					if isFeature {
						fmt.Printf("Issue #%d is a feature\n", cardIssueNumber)
						featureIssueNumbers = append(featureIssueNumbers, cardIssueNumber)
					} else {
						// Find parent feature
						parentFeature, err := s.findParentFeature(cardIssueNumber)
						if err != nil {
							log.Printf("Error finding parent feature: %v", err)
						} else if parentFeature > 0 {
							fmt.Printf("Found parent feature #%d for issue #%d\n", parentFeature, cardIssueNumber)
							featureIssueNumbers = append(featureIssueNumbers, parentFeature)
						}
					}
				}
			}
		}
	} else {
		fmt.Println("No issue context available, checking all features in the repository")

		// Search for all open feature issues
		query := fmt.Sprintf("repo:%s/%s is:issue state:open label:feature",
			s.config.Owner, s.config.Repo)

		searchOptions := &github.SearchOptions{
			ListOptions: github.ListOptions{PerPage: 100},
		}

		searchResults, _, err := s.restClient.Search.Issues(s.ctx, query, searchOptions)
		if err != nil {
			log.Printf("Error searching for features: %v", err)
		} else if searchResults.GetTotal() > 0 {
			fmt.Printf("Found %d feature issues to process\n", searchResults.GetTotal())
			for _, item := range searchResults.Issues {
				featureIssueNumbers = append(featureIssueNumbers, item.GetNumber())
			}
		} else {
			fmt.Println("No features found in repository")
			fmt.Println("For manual runs, please provide a FEATURE_ISSUE_NUMBER environment variable")
		}
	}

	// Update specified milestone if provided
	if s.config.MilestoneNumber > 0 {
		fmt.Printf("Updating milestone #%d\n", s.config.MilestoneNumber)
		if err := s.updateMilestone(s.config.MilestoneNumber); err != nil {
			log.Printf("Error updating milestone #%d: %v", s.config.MilestoneNumber, err)
		}
	}

	// Process features
	fmt.Printf("Found %d feature issues to update\n", len(featureIssueNumbers))

	for _, featureNumber := range featureIssueNumbers {
		milestoneNumberFromFeature, err := s.updateFeatureIssue(featureNumber)
		if err != nil {
			log.Printf("Error updating feature #%d: %v", featureNumber, err)
			continue
		}

		if milestoneNumberFromFeature > 0 && milestoneNumberFromFeature != s.config.MilestoneNumber {
			if err := s.updateMilestone(milestoneNumberFromFeature); err != nil {
				log.Printf("Error updating milestone #%d: %v", milestoneNumberFromFeature, err)
			}
		}
	}

	fmt.Println("Date update process complete")
	return nil
}

func (s *DateManagerService) getIssue(issueNumber int) (*github.Issue, error) {
	if issue, ok := s.issueCache[issueNumber]; ok {
		return issue, nil
	}

	issue, _, err := s.restClient.Issues.Get(s.ctx, s.config.Owner, s.config.Repo, issueNumber)
	if err != nil {
		return nil, err
	}

	s.issueCache[issueNumber] = issue
	return issue, nil
}

// Cached milestone retrieval
func (s *DateManagerService) getMilestone(milestoneNumber int) (*github.Milestone, error) {
	if milestone, ok := s.milestoneCache[milestoneNumber]; ok {
		return milestone, nil
	}

	milestone, _, err := s.restClient.Issues.GetMilestone(s.ctx, s.config.Owner, s.config.Repo, milestoneNumber)
	if err != nil {
		return nil, err
	}

	s.milestoneCache[milestoneNumber] = milestone
	return milestone, nil
}

// Cached project data retrieval
func (s *DateManagerService) getProjectDataCached(issueNumber int) ([]ProjectV2Item, error) {
	if items, ok := s.projectDataCache[issueNumber]; ok {
		return items, nil
	}

	items, err := s.getProjectData(issueNumber)
	if err != nil {
		return nil, err
	}

	s.projectDataCache[issueNumber] = items
	return items, nil
}

// Cached project field IDs retrieval
func (s *DateManagerService) getProjectFieldIdsCached(projectId string) (FieldIDs, error) {
	if fieldIds, ok := s.projectFieldCache[projectId]; ok {
		return fieldIds, nil
	}

	fieldIds, err := s.getProjectFieldIds(projectId)
	if err != nil {
		return FieldIDs{}, err
	}

	s.projectFieldCache[projectId] = fieldIds
	return fieldIds, nil
}

// Find the parent feature for a task
func (s *DateManagerService) findParentFeature(taskIssueNumber int) (int, error) {
	fmt.Printf("Finding parent feature for task #%d...\n", taskIssueNumber)

	// Get the task issue details
	issue, err := s.getIssue(taskIssueNumber)
	if err != nil {
		return 0, err
	}

	// Check if it mentions a parent in the body
	featureRegex := regexp.MustCompile(`(?i)(?:part of|belongs to|child of|task of|related to|parent:|feature:) #(\d+)`)

	if issue.Body != nil {
		matches := featureRegex.FindStringSubmatch(*issue.Body)
		if len(matches) > 1 {
			featureNumber, _ := strconv.Atoi(matches[1])

			// Verify this is actually a feature
			feature, err := s.getIssue(featureNumber)
			if err != nil {
				return 0, err
			}

			// Check for feature label
			isFeature := false
			for _, label := range feature.Labels {
				if strings.ToLower(*label.Name) == "feature" {
					isFeature = true
					break
				}
			}

			if isFeature {
				return featureNumber, nil
			}
		}
	}

	// Check timeline events for references
	timelineEvents, _, err := s.restClient.Issues.ListIssueTimeline(s.ctx, s.config.Owner, s.config.Repo, taskIssueNumber, &github.ListOptions{})
	if err != nil {
		return 0, err
	}

	for _, event := range timelineEvents {
		if event.GetEvent() == "cross-referenced" {
			source := event.GetSource()
			if source != nil && source.GetIssue() != nil {
				potentialFeatureNumber := source.GetIssue().GetNumber()

				// Verify this is a feature
				potentialFeature, _, err := s.restClient.Issues.Get(s.ctx, s.config.Owner, s.config.Repo, potentialFeatureNumber)
				if err != nil {
					continue
				}

				// Check for feature label
				isFeature := false
				for _, label := range potentialFeature.Labels {
					if strings.ToLower(*label.Name) == "feature" {
						isFeature = true
						break
					}
				}

				if isFeature {
					return potentialFeatureNumber, nil
				}
			}
		}
	}

	return 0, nil
}

// Find all tasks for a feature issue
func (s *DateManagerService) findFeatureTasks(featureNumber int) ([]int, error) {
	fmt.Printf("Finding tasks for feature #%d...\n", featureNumber)
	tasks := []int{}

	// Get the feature issue to check for task references in the body
	feature, _, err := s.restClient.Issues.Get(s.ctx, s.config.Owner, s.config.Repo, featureNumber)
	if err != nil {
		return nil, err
	}

	// Look for task list items with issue references in the body
	taskListRegex := regexp.MustCompile(`-\s*\[\s*[x ]?\s*\]\s*#(\d+)`)

	if feature.Body != nil {
		matches := taskListRegex.FindAllStringSubmatch(*feature.Body, -1)
		for _, match := range matches {
			if len(match) > 1 {
				taskNumber, _ := strconv.Atoi(match[1])
				if !contains(tasks, taskNumber) {
					tasks = append(tasks, taskNumber)
				}
			}
		}
	}

	// Search for issues that reference this feature
	query := fmt.Sprintf("repo:%s/%s is:issue state:open %d in:body -label:feature",
		s.config.Owner, s.config.Repo, featureNumber)

	searchOptions := &github.SearchOptions{
		ListOptions: github.ListOptions{PerPage: 100},
	}

	searchResults, _, err := s.restClient.Search.Issues(s.ctx, query, searchOptions)
	if err != nil {
		log.Printf("Error searching for tasks that reference feature #%d: %v", featureNumber, err)
	} else {
		for _, item := range searchResults.Issues {
			if item.GetNumber() != featureNumber && !contains(tasks, item.GetNumber()) {
				// Check if this issue references the feature in a way that indicates it's a task
				potentialTask, _, err := s.restClient.Issues.Get(s.ctx, s.config.Owner, s.config.Repo, item.GetNumber())
				if err != nil {
					continue
				}

				// Check for task label
				isTask := false
				for _, label := range potentialTask.Labels {
					if strings.ToLower(*label.Name) == "task" {
						isTask = true
						break
					}
				}

				// Or check for task-like references to the feature
				isTaskByRef := false
				if potentialTask.Body != nil {
					taskRefRegex := regexp.MustCompile(fmt.Sprintf(`(?i)(?:part of|belongs to|child of|task of|related to|parent:|feature:) #%d\b`, featureNumber))
					isTaskByRef = taskRefRegex.MatchString(*potentialTask.Body)
				}

				if isTask || isTaskByRef {
					tasks = append(tasks, item.GetNumber())
				}
			}
		}
	}

	fmt.Printf("Found %d tasks for feature #%d\n", len(tasks), featureNumber)
	return tasks, nil
}

// Get project field values for an issue
func (s *DateManagerService) getProjectData(issueNumber int) ([]ProjectV2Item, error) {
	// Define the variables
	variables := map[string]interface{}{
		"owner":       githubv4.String(s.config.Owner),
		"repo":        githubv4.String(s.config.Repo),
		"issueNumber": githubv4.Int(issueNumber),
	}

	// Define the query structure
	var query struct {
		Repository struct {
			Issue struct {
				ProjectItems struct {
					Nodes []struct {
						ID      string `graphql:"id"`
						Project struct {
							ID    string `graphql:"id"`
							Title string `graphql:"title"`
						} `graphql:"project"`
						FieldValues struct {
							Nodes []struct {
								TypeName string `graphql:"__typename"`
								// Use appropriate fragments for each possible type in the union
								// For date values
								DateValue struct {
									Date  string `graphql:"date"`
									Field struct {
										// We need to use a fragment here too if Field is a union or interface
										CommonField struct {
											Name string `graphql:"name"`
										} `graphql:"... on ProjectV2FieldCommon"`
									} `graphql:"field"`
								} `graphql:"... on ProjectV2ItemFieldDateValue"`
								// Add other value types as needed
							} `graphql:"nodes"`
						} `graphql:"fieldValues(first: 20)"`
					} `graphql:"nodes"`
				} `graphql:"projectItems(first: 20)"`
			} `graphql:"issue(number: $issueNumber)"`
		} `graphql:"repository(owner: $owner, name: $repo)"`
	}

	// Execute the query
	err := s.graphqlClient.Query(s.ctx, &query, variables)
	if err != nil {
		return nil, err
	}

	// Convert to your ProjectV2Item type
	var result []ProjectV2Item
	// Transform the data as needed

	return result, nil
}

// Get next business day (skip weekends)
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

// Format date as YYYY-MM-DD
func formatDate(date time.Time) githubv4.Date {
	return githubv4.Date{Time: date}
}

// Parse date from YYYY-MM-DD format
func parseDate(dateStr string) (githubv4.Date, error) {
	date, err := time.Parse("2006-01-02", dateStr)
	if err != nil {
		return githubv4.Date{}, err
	}

	return githubv4.Date{Time: date}, nil
}

// Find project field IDs for a project
func (s *DateManagerService) getProjectFieldIds(projectId string) (FieldIDs, error) {
	var query struct {
		Node struct {
			Fields struct {
				Nodes []ProjectV2Field
			} `graphql:"fields(first: 50)"`
		} `graphql:"node(id: $projectId)"`
	}

	variables := map[string]interface{}{
		"projectId": githubv4.ID(projectId),
	}

	err := s.graphqlClient.Query(s.ctx, &query, variables)
	if err != nil {
		return FieldIDs{}, err
	}

	fieldIds := FieldIDs{}
	for _, field := range query.Node.Fields.Nodes {
		switch field.Name {
		case "Start Date":
			fieldIds.StartDateFieldID = field.ID
		case "Target Date":
			fieldIds.TargetDateFieldID = field.ID
		case "Duration (days)":
			fieldIds.DurationFieldID = field.ID
		}
	}

	return fieldIds, nil
}

// Update a date field for an issue in a project
func (s *DateManagerService) updateProjectDateField(projectId string, itemId string, fieldId string, date githubv4.Date) error {
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
			Date: &date,
		},
	}

	return s.graphqlClient.Mutate(s.ctx, &mutation, input, nil)
}

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

// Update a feature issue based on its tasks
func (s *DateManagerService) updateFeatureIssue(featureNumber int) (int, error) {
	fmt.Printf("\n--- Updating feature #%d ---\n", featureNumber)

	// Get the feature's milestone
	feature, err := s.getIssue(featureNumber)
	if err != nil {
		return 0, err
	}

	milestoneNumber := 0
	if feature.Milestone != nil {
		milestoneNumber = feature.Milestone.GetNumber()
	}

	// Find all tasks for this feature
	taskNumbers, err := s.findFeatureTasks(featureNumber)
	if err != nil {
		return milestoneNumber, err
	}

	if len(taskNumbers) == 0 {
		fmt.Printf("No tasks found for feature #%d, skipping date updates\n", featureNumber)
		return milestoneNumber, nil
	}

	// Track earliest start and latest target dates across all tasks
	var earliestStartDate time.Time
	var latestTargetDate time.Time

	// Keep track of tasks with missing dates
	tasksWithMissingDates := []TaskWithMissingDates{}

	// Get date information from each task
	for _, taskNumber := range taskNumbers {
		projectItems, err := s.getProjectDataCached(taskNumber)
		if err != nil {
			log.Printf("Error getting project data for task #%d: %v", taskNumber, err)
			continue
		}

		// If task is not in any project, track it
		if len(projectItems) == 0 {
			tasksWithMissingDates = append(tasksWithMissingDates, TaskWithMissingDates{
				TaskNumber:   taskNumber,
				ProjectItems: []ProjectV2Item{},
			})
			continue
		}

		taskHasDates := false

		for _, projectItem := range projectItems {
			var startDate *githubv4.Date
			var targetDate *githubv4.Date
			var duration *float64

			for _, fieldValue := range projectItem.FieldValues.Nodes {
				if fieldValue.Field.Name == "Start Date" && fieldValue.DateValue != nil {
					startDate = fieldValue.DateValue
				} else if fieldValue.Field.Name == "Target Date" && fieldValue.DateValue != nil {
					targetDate = fieldValue.DateValue
				} else if fieldValue.Field.Name == "Duration (days)" && fieldValue.NumberValue != nil {
					duration = fieldValue.NumberValue
				}
			}

			if startDate != nil {
				if earliestStartDate.IsZero() || startDate.Before(earliestStartDate) {
					earliestStartDate = startDate.Time
				}
				taskHasDates = true
			}

			if targetDate != nil {
				if latestTargetDate.IsZero() || targetDate.After(latestTargetDate) {
					latestTargetDate = targetDate.Time
				}
				taskHasDates = true
			}

			fmt.Println(duration)
		}

		if !taskHasDates {
			tasksWithMissingDates = append(tasksWithMissingDates, TaskWithMissingDates{
				TaskNumber:   taskNumber,
				ProjectItems: projectItems,
			})
		}
	}

	// If no tasks have dates, set default start date to today
	if earliestStartDate.IsZero() {
		earliestStartDate = getNextBusinessDay(time.Now())
		fmt.Printf("No start dates found in tasks. Using today (%s) as the default start date.\n", formatDate(earliestStartDate))
	}

	// If we still need to calculate target dates for tasks with missing dates
	if len(tasksWithMissingDates) > 0 {
		fmt.Printf("Found %d tasks with missing dates. Setting default dates...\n", len(tasksWithMissingDates))

		var currentDate time.Time
		if !latestTargetDate.IsZero() {
			currentDate = latestTargetDate.AddDate(0, 0, 1)
		} else {
			currentDate = earliestStartDate.AddDate(0, 0, 1)
		}

		for _, task := range tasksWithMissingDates {
			// Add one business day for each task
			currentDate = getNextBusinessDay(currentDate)

			taskStartDate := formatDate(currentDate)
			currentDate = currentDate.AddDate(0, 0, 1)
			taskTargetDate := formatDate(getNextBusinessDay(currentDate))

			fmt.Printf("Setting default dates for task #%d: Start=%s, Target=%s\n", task.TaskNumber, taskStartDate, taskTargetDate)

			startDateParsed := taskStartDate.Time
			targetDateParsed := taskTargetDate.Time

			// Update the task's dates in all projects
			for _, projectItem := range task.ProjectItems {
				fieldIds, err := s.getProjectFieldIdsCached(projectItem.Project.ID)
				if err != nil {
					log.Printf("Error getting field IDs: %v", err)
					continue
				}

				if fieldIds.StartDateFieldID != "" {
					err := s.updateProjectDateField(projectItem.Project.ID, projectItem.ID, fieldIds.StartDateFieldID, formatDate(startDateParsed))
					if err != nil {
						log.Printf("Error updating start date: %v", err)
					}
				}

				if fieldIds.TargetDateFieldID != "" {
					err := s.updateProjectDateField(projectItem.Project.ID, projectItem.ID, fieldIds.TargetDateFieldID, formatDate(targetDateParsed))
					if err != nil {
						log.Printf("Error updating target date: %v", err)
					}
				}

				// Add this new block for duration update
				if fieldIds.DurationFieldID != "" {
					// Calculate duration in days (including the target date)
					duration := float64(targetDateParsed.Sub(startDateParsed).Hours()/24) + 1
					if duration < 1 {
						duration = 1 // Minimum duration of 1 day
					}

					fmt.Printf("Setting duration for task #%d to %.0f days\n", task.TaskNumber, duration)
					err := s.updateProjectDurationField(projectItem.Project.ID, projectItem.ID, fieldIds.DurationFieldID, githubv4.Float(duration))
					if err != nil {
						log.Printf("Error updating duration: %v", err)
					}
				}
			}

			// Update our tracking of latest target date
			if latestTargetDate.IsZero() || targetDateParsed.After(latestTargetDate) {
				latestTargetDate = targetDateParsed
			}
		}
	}

	// Now update the feature's dates
	featureProjectItems, err := s.getProjectData(featureNumber)
	if err != nil {
		return milestoneNumber, err
	}

	for _, projectItem := range featureProjectItems {
		fieldIds, err := s.getProjectFieldIds(projectItem.Project.ID)
		if err != nil {
			log.Printf("Error getting field IDs: %v", err)
			continue
		}

		if fieldIds.StartDateFieldID != "" && !earliestStartDate.IsZero() {
			fmt.Printf("Updating feature #%d start date to %s\n", featureNumber, formatDate(earliestStartDate))
			err := s.updateProjectDateField(projectItem.Project.ID, projectItem.ID, fieldIds.StartDateFieldID, formatDate(earliestStartDate))
			if err != nil {
				log.Printf("Error updating feature start date: %v", err)
			}
		}

		if fieldIds.TargetDateFieldID != "" && !latestTargetDate.IsZero() {
			fmt.Printf("Updating feature #%d target date to %s\n", featureNumber, formatDate(latestTargetDate))
			err := s.updateProjectDateField(projectItem.Project.ID, projectItem.ID, fieldIds.TargetDateFieldID, formatDate(latestTargetDate))
			if err != nil {
				log.Printf("Error updating feature target date: %v", err)
			}
		}

		// Add this new block for feature duration update
		if fieldIds.DurationFieldID != "" && !earliestStartDate.IsZero() && !latestTargetDate.IsZero() {
			// Calculate duration in days (including the target date)
			duration := float64(latestTargetDate.Sub(earliestStartDate).Hours()/24) + 1
			if duration < 1 {
				duration = 1 // Minimum duration of 1 day
			}

			fmt.Printf("Updating feature #%d duration to %.0f days\n", featureNumber, duration)
			err := s.updateProjectDurationField(projectItem.Project.ID, projectItem.ID, fieldIds.DurationFieldID, githubv4.Float(duration))
			if err != nil {
				log.Printf("Error updating feature duration: %v", err)
			}
		}
	}

	return milestoneNumber, nil
}

// Update milestone due date based on issues in the milestone
func (s *DateManagerService) updateMilestone(milestoneNumber int) error {
	fmt.Printf("\n--- Updating milestone #%d ---\n", milestoneNumber)

	// Get all issues in this milestone
	issuesOptions := &github.IssueListByRepoOptions{
		Milestone: strconv.Itoa(milestoneNumber),
		State:     "open",
		ListOptions: github.ListOptions{
			PerPage: 100,
		},
	}

	issues, _, err := s.restClient.Issues.ListByRepo(s.ctx, s.config.Owner, s.config.Repo, issuesOptions)
	if err != nil {
		return err
	}

	if len(issues) == 0 {
		fmt.Printf("No open issues found for milestone #%d\n", milestoneNumber)
		return nil
	}

	// Find the latest target date from all issues
	var latestTargetDate time.Time

	for _, issue := range issues {
		projectItems, err := s.getProjectDataCached(issue.GetNumber())
		if err != nil {
			log.Printf("Error getting project data for issue #%d: %v", issue.GetNumber(), err)
			continue
		}

		for _, projectItem := range projectItems {
			for _, fieldValue := range projectItem.FieldValues.Nodes {
				if fieldValue.Field.Name == "Target Date" && fieldValue.DateValue != nil {
					parsedDate := fieldValue.DateValue.Time
					if latestTargetDate.IsZero() || parsedDate.After(latestTargetDate) {
						latestTargetDate = parsedDate
					}
				}
			}
		}
	}

	if !latestTargetDate.IsZero() {
		// Update the milestone due date
		fmt.Printf("Setting milestone #%d due date to %s\n", milestoneNumber, formatDate(latestTargetDate))
		milestone := &github.Milestone{
			DueOn: &github.Timestamp{Time: latestTargetDate},
		}
		_, _, err := s.restClient.Issues.EditMilestone(s.ctx, s.config.Owner, s.config.Repo, milestoneNumber, milestone)
		if err != nil {
			return err
		}
	} else {
		fmt.Printf("No target dates found for issues in milestone #%d\n", milestoneNumber)
	}

	return nil
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
