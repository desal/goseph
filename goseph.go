package main

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

var (
	verbose       = false
	goInstall     = false
	escapeWindows = false
	stdLibs       map[string]struct{}
)

const ussr = "\x1b[31m☭\x1b[0m"

type GitStatus int

const (
	GitStatus_Clean       = 1
	GitStatus_Uncommitted = 2
	GitStatus_Detached    = 3
	GitStatus_Unpushed    = 4
)

type GitProject struct {
	GoImport        string
	RemoteOriginUrl string
	Branch          string `json:"-"`
	SHA             string
	GitStatus       GitStatus `json:"-"`
}

func (p *GitProject) dependency() {}

type StandardLib struct{}

func (s *StandardLib) dependency() {}

type SamePkg struct{}

func (s *SamePkg) dependency() {}

type Dependency interface {
	dependency()
}

func (s StandardLib) String() string {
	return "StandardLib{}"
}

func (s SamePkg) String() string {
	return "SamePkg{}"
}

func (g GitProject) String() string {
	return fmt.Sprintf("GitProject{Import: '%s', Url: '%s', SHA: '%s', Branch: '%s', Status: %v}",
		g.GoImport, g.RemoteOriginUrl, g.SHA, g.Branch, g.GitStatus)
}

func checkPath(path string) bool {
	if _, err := os.Stat(path); err == nil {
		return true
	}
	return false
}

func runCmd(dir, command string, args ...string) (string, []byte, error) {
	msg := fmt.Sprintf(" [%s] %s %s %s", dir, ussr, command, strings.Join(args, " "))
	if verbose {
		fmt.Println(msg)
	}

	cmd := exec.Command(command, args...)
	cmd.Dir = dir
	output, err := cmd.CombinedOutput()
	return msg, output, err
}

func errorExit(output string) {
	fmt.Printf(" %s Error %s\n", ussr, ussr)
	fmt.Println("Output:")
	fmt.Println(output)
	panic("error")
}

func mustRunCmd(dir, command string, args ...string) []byte {
	msg, output, err := runCmd(dir, command, args...)
	if err != nil {
		if !verbose {
			fmt.Println(msg)
		}
		errorExit(string(output))
	}
	return output
}

func getStdLibs() map[string]struct{} {
	stdListOutput := mustRunCmd(".", "go", "list", "std")
	stdLibs := strings.Split(string(stdListOutput), "\n")
	result := map[string]struct{}{}
	for _, lib := range stdLibs {
		if lib != "" {
			result[lib] = struct{}{}
		}
	}
	return result
}

func isStdLib(lib string) bool {
	_, result := stdLibs[lib]
	return result
}

func gitIsClean(gitPath string) GitStatus {
	unchecked := mustRunCmd(gitPath, "git", "status", "--porcelain")
	if len(unchecked) != 0 {
		return GitStatus_Uncommitted
	}

	//Detached head check
	if _, _, err := runCmd(gitPath, "git", "symbolic-ref", "HEAD"); err != nil {
		return GitStatus_Detached
	}

	var unsynced []byte
	var attemptEscape bool

	//Some (but not all) versions of git on windows like the curly brackets escaped.
	//Rather than attempting to decipher this from the version of git installed, I just retry the first time this
	//runs with an escape, if that works, then we switch over for remaining invokations
	if !escapeWindows {
		var err error
		_, unsynced, err = runCmd(gitPath, "git", "rev-list", `HEAD@{upstream}..HEAD`)
		if err != nil {
			attemptEscape = true
		}
	}

	if escapeWindows || attemptEscape {
		unsynced = mustRunCmd(gitPath, "git", "rev-list", `HEAD@'{'upstream'}'..HEAD`)

		if attemptEscape {
			escapeWindows = true
			if verbose {
				fmt.Println("  Enabled escaping { and }")
			}
		}
	}
	if len(unsynced) == 0 {
		return GitStatus_Clean
	}
	return GitStatus_Unpushed
}

//If we're on a specific commit and we want to update to head. Will only allow us to proceed if everything is comitted
//(i.e. pre-emptively catch git giving us grief)
func gitToMaster(gitPath string) {
	unchecked := mustRunCmd(gitPath, "git", "status", "--porcelain")
	if len(unchecked) != 0 {
		fmt.Printf("%s has uncommitted changes, can't checkout master")
		os.Exit(1)
	}
	_ = mustRunCmd(gitPath, "git", "checkout", "master")
}

// Populates .RemoteOriginUrl, .GitStatus, .SHA and .Branch (i.e. does not touch .GoImport)
func gitDetails(workingDir string) (result *GitProject) {
	result = new(GitProject)
	msg, urlOutput, err := runCmd(workingDir, "git", "config", "--get", "remote.origin.url")
	if err != nil {
		if _, ok := err.(*exec.ExitError); !ok {
			fmt.Println(msg)
			errorExit(string(urlOutput))
		}
	} else {
		result.RemoteOriginUrl = strings.Split(string(urlOutput), "\n")[0]
		result.GitStatus = gitIsClean(workingDir)
	}

	shaOutput := mustRunCmd(workingDir, "git", "rev-parse", "HEAD")
	result.SHA = strings.Split(string(shaOutput), "\n")[0]

	branchOutput := mustRunCmd(workingDir, "git", "rev-parse", "--abbrev-ref", "HEAD")
	result.Branch = strings.Split(string(branchOutput), "\n")[0]

	return
}

func getProjects(goPath string) []*GitProject {
	findOutput := mustRunCmd(goPath, "find", "-name", ".git", "-type", "d", "-prune")
	paths := strings.Split(string(findOutput), "\n")

	projects := make([]*GitProject, len(paths)-1)

	for i := 0; i < len(projects); i++ {
		relativePath := paths[i][:len(paths[i])-5]
		workingDir := filepath.Join(goPath, relativePath)
		projects[i] = gitDetails(workingDir)
		projects[i].GoImport = relativePath[6:]

	}
	return projects
}

func getGoList(cmd, pkg string) []string {
	args := []string{"list", "-f", fmt.Sprintf(`{{join .%s "\n"}}`, cmd)}
	if pkg != "" {
		args = append(args, pkg)
	}

	goListOutput := mustRunCmd(".", "go", args...)

	result := strings.Split(string(goListOutput), "\n")
	return result[:len(result)-1]
}

func removeDupes(strs []string) []string {
	m := map[string]struct{}{}
	for _, s := range strs {
		m[s] = struct{}{}
	}
	result := []string{}
	for s, _ := range m {
		result = append(result, s)
	}
	return result
}

func getGitProject(projects []*GitProject, dep string) *GitProject {
	for _, project := range projects {
		if strings.HasPrefix(dep, project.GoImport) {
			return project
		}
	}
	return nil

}

func getDetails(projects []*GitProject, dep string) Dependency {
	importIsStd := isStdLib(dep)
	if importIsStd {
		return &StandardLib{}
	}
	return getGitProject(projects, dep)
}

func allDeps(gopath string, projects []*GitProject) (deps, testDeps map[string]Dependency) {
	workingDir, err := os.Getwd()
	if err != nil {
		panic(err)
	}

	currentPkg, err := filepath.Rel(filepath.Join(gopath, "src"), workingDir)
	if err != nil {
		panic(err)
	}
	currentPkg = filepath.ToSlash(currentPkg)

	if verbose {
		fmt.Printf(" currentPkgGoImport = %s\n", currentPkg)
	}

	deps = map[string]Dependency{}
	for _, dep := range removeDupes(getGoList("Deps", "./...")) {
		if !strings.HasPrefix(dep, currentPkg) {
			deps[dep] = getDetails(projects, dep)
		} else {
			deps[dep] = &SamePkg{}
		}
	}

	testDeps = map[string]Dependency{}

	for _, testImport := range removeDupes(getGoList("TestImports", "./...")) {
		if _, covered := deps[testImport]; !covered {
			if !strings.HasPrefix(testImport, currentPkg) {
				testDeps[testImport] = getDetails(projects, testImport)
			} else {
				deps[testImport] = &SamePkg{}
			}

			testImportDeps := getGoList("Deps", testImport)
			for _, testImportDep := range testImportDeps {
				if _, covered := deps[testImportDep]; !covered {
					if !strings.HasPrefix(testImportDep, currentPkg) {
						testDeps[testImportDep] = getDetails(projects, testImportDep)
					} else {
						testDeps[testImportDep] = &SamePkg{}
					}

				}
			}
		}
	}

	return deps, testDeps
}

func Snapshot(allDeps map[string]Dependency) ([]*GitProject, bool) {
	outputDeps := []*GitProject{}
	canOutput := true

	//Create a map with import root as key, so if we have git repo a/, and there's imports of a/b and a/c
	//that we only process once.

	gitDeps := map[string]*GitProject{}

	for importName, dependency := range allDeps {
		switch dependency := dependency.(type) {
		case *GitProject:
			gitDeps[dependency.GoImport] = dependency
		case *StandardLib:
		case *SamePkg:
		default:
			fmt.Printf("Import '%s' is neither a standard library nor part of a git project (%T)\n", importName, dependency)
			canOutput = false
		}
	}

	for _, dependency := range gitDeps {
		if dependency.RemoteOriginUrl == "" {
			fmt.Printf("Snapshot '%s' failed. No remote URL.\n", dependency.GoImport)
			canOutput = false
		} else if dependency.Branch != "master" {
			fmt.Printf("Snapshot '%s' failed. Branch is not master (%s).\n",
				dependency.GoImport, dependency.Branch)
			canOutput = false
		} else if dependency.GitStatus == GitStatus_Detached {
			fmt.Printf("Snapshot '%s' failed. Detached git head.\n",
				dependency.GoImport)
			canOutput = false
		} else if dependency.GitStatus == GitStatus_Uncommitted {
			fmt.Printf("Snapshot '%s' failed. Uncomitted git changes.\n",
				dependency.GoImport)
			canOutput = false
		} else if dependency.GitStatus == GitStatus_Unpushed {
			fmt.Printf("Snapshot '%s' failed. Unpushed git changes.\n",
				dependency.GoImport)
			canOutput = false
		} else {
			outputDeps = append(outputDeps, dependency)
		}
	}

	return outputDeps, canOutput
}

func RecursiveUpdate(gopath, gitCacheFile, cmd string, dependencies []*GitProject, gitCache map[string]*GitProject) {
	workingFile := gitCacheFile
	if workingFile == "" {
		workingFile = tempGitCache()
	}
	writeGitCache(workingFile, gitCache)

	for _, dep := range dependencies {
		importPath := filepath.Join(gopath, "src", dep.GoImport)

		args := []string{}
		args = append(args, "--git-cache="+workingFile)
		args = append(args, "--ignore-missing")
		if verbose {
			args = append(args, "-v")
		}
		if goInstall {
			args = append(args, "--install")
		}
		args = append(args, cmd)
		_ = mustRunCmd(importPath, "goseph", args...)
	}

	if gitCacheFile == "" {
		err := os.Remove(workingFile)
		if err != nil {
			panic(err)
		}
	}
}

func UpdateAll(gopath string, jsonDependencies []*GitProject, gitCacheFile string) {
	gitCache := readGitCache(gitCacheFile)
	updated := []*GitProject{}

	for _, dependency := range jsonDependencies {
		importPath := filepath.Join(gopath, "src", dependency.GoImport)
		if !checkPath(importPath) {
			fmt.Printf("%s does not exist, cloning\n", dependency.GoImport)
			cloneDependency(importPath, dependency)
			updated = append(updated, dependency)
			continue
		}

		if _, hasCache := gitCache[importPath]; hasCache {
			if verbose {
				fmt.Printf("Skipping %s, already handled\n", importPath)
			}
			continue
		}

		currentDetails := gitDetails(importPath)
		if currentDetails.GitStatus == GitStatus_Detached {
			gitToMaster(importPath)
			currentDetails = gitDetails(importPath)
		}
		gitCache[importPath] = currentDetails

		if currentDetails.RemoteOriginUrl == "" {
			fmt.Printf("Skipping %s. No remote URL.\n", dependency.GoImport)
		} else if currentDetails.Branch != "master" {
			fmt.Printf("Skipping %s. Remote branch is not master (%s).\n",
				dependency.GoImport, currentDetails.Branch)
		} else if currentDetails.GitStatus != GitStatus_Clean {
			fmt.Printf("Skipping %s. git status is unclean.\n", dependency.GoImport)
		} else {
			_ = mustRunCmd(importPath, "git", "pull")
			if goInstall {
				_ = mustRunCmd(importPath, "go", "install", "./...")
			}
			updated = append(updated, dependency)
		}
	}

	RecursiveUpdate(gopath, gitCacheFile, "update", updated, gitCache)

}

func cloneDependency(importPath string, dependency *GitProject) {
	dir, file := filepath.Split(importPath)
	err := os.MkdirAll(dir, 0755)
	if err != nil {
		panic(err)
	}

	_ = mustRunCmd(dir, "git", "clone", dependency.RemoteOriginUrl, file)
	if goInstall {
		_ = mustRunCmd(importPath, "go", "install", "./...")
	}
}

func CloneMissing(gopath string, jsonDependencies []*GitProject, cacheFile string) {
	cloned := []*GitProject{}

	for _, dependency := range jsonDependencies {
		importPath := filepath.Join(gopath, "src", dependency.GoImport)
		if checkPath(importPath) {
			fmt.Printf("Skipping %s, path already exists\n", dependency.GoImport)
			continue
		}
		cloneDependency(importPath, dependency)
		cloned = append(cloned, dependency)
	}
	RecursiveUpdate(gopath, cacheFile, "fetch", cloned, map[string]*GitProject{})
}

func Reproduce(gopath string, jsonDependencies []*GitProject) {
	for _, dependency := range jsonDependencies {
		importPath := filepath.Join(gopath, "src", dependency.GoImport)
		if checkPath(importPath) {
			fmt.Printf("%s, path already exists. Aborting\n", dependency.GoImport)
			os.Exit(1)
		}
		dir, file := filepath.Split(importPath)
		err := os.MkdirAll(dir, 0755)
		if err != nil {
			panic(err)
		}

		_ = mustRunCmd(dir, "git", "clone", dependency.RemoteOriginUrl, file)
		_ = mustRunCmd(importPath, "git", "checkout", dependency.SHA)
		if goInstall {
			_ = mustRunCmd(importPath, "go", "install", "./...")
		}
	}
}

func Status(deps, testDeps map[string]Dependency) {

	fmt.Println("Direct Dependencies:")
	for dep, details := range deps {
		fmt.Println("  ", dep, details)
	}

	fmt.Println("\nAdditional for testing only:")
	for dep, details := range testDeps {
		fmt.Println("  ", dep, details)
	}
}

func combinedDeps(deps, testDeps map[string]Dependency) map[string]Dependency {
	result := map[string]Dependency{}
	for name, dependency := range deps {
		result[name] = dependency
	}

	for name, dependency := range testDeps {
		result[name] = dependency
	}

	return result
}

func readJson(filename string, ignoreMissing bool) (deps []*GitProject, testDeps []*GitProject) {
	file, err := ioutil.ReadFile(filename)
	if err != nil {
		if _, isPathError := err.(*os.PathError); isPathError && ignoreMissing {
			if verbose {
				fmt.Println("[%s] Ignoring missing gsdeps.json\n", filename)
			}
			return nil, nil
		}
		panic(err)
	}
	result := map[string][]*GitProject{}

	json.Unmarshal(file, &result)

	return result["Build"], result["Test"]
}

func writeJson(filename string, depsSnapshot []*GitProject, testDepsSnapshot []*GitProject) {
	m := map[string][]*GitProject{
		"Build": depsSnapshot,
		"Test":  testDepsSnapshot,
	}

	jsonOutput, err := json.MarshalIndent(&m, "", "  ")
	if err != nil {
		panic(err)
	}

	err = ioutil.WriteFile(filename, jsonOutput, 0644)
	if err != nil {
		panic(err)
	}
}

func readGitCache(filename string) map[string]*GitProject {

	if filename == "" {
		return map[string]*GitProject{}
	}

	file, err := ioutil.ReadFile(filename)
	if err != nil {
		if _, isPathError := err.(*os.PathError); isPathError {
			return map[string]*GitProject{}
		}
		panic(err)
	}
	var result map[string]*GitProject
	json.Unmarshal(file, &result)

	return result
}

func writeGitCache(filename string, gitCache map[string]*GitProject) {
	if filename == "" {
		return
	}

	jsonOutput, err := json.MarshalIndent(&gitCache, "", "  ")
	if err != nil {
		panic(err)
	}

	err = ioutil.WriteFile(filename, jsonOutput, 0644)
	if err != nil {
		panic(err)
	}
}

func tempGitCache() string {
	f, err := ioutil.TempFile(os.TempDir(), "goseph-git-cache")
	if err != nil {
		panic(err)
	}
	gitCache := f.Name()
	f.Close()
	return gitCache
}

func Usage() {

	fmt.Printf(`
 %s Goseph Instalin - The socialist gopher's dependency manager %s

Usage: goseph [option...] command

Options
  -v, --verbose
             Verbose

  -m, --ignore-missing
             A missing gsdeps.json is considered an empty set of dependencies.

  -i, --install
             runs go install ./... after git checkout

  -t, --tests
             fetches deps required to run tests

      --git-cache=file
             For use in scripting/recursion, uses a json file containing
             current git repo information, to prevent costly git operations.
             (File is modified in place).

Commands

  status     Shows all dependencies and their statuses

  snapshot   Scans currently used dependencies and outputs to gsdeps.json
			
  fetch      Fetches any missing dependencies
              - Skips over any present dependencies
              - Checks out HEAD
              - Updates transitive dependencies

  update     Updates all dependencies, as with fetch except:
              - Updates any present dependency if on HEAD
                (including transisitive dependencies)

  reproduce  Reproduce all dependencies to the SHA in snapshot deps.json 
              - Packages already present are considered to be errors.


If a dependency is corrupt, or otherwise unusable, the easiest way to recover
is to delete the directory and run goseph fetch again.
`, ussr, ussr)
}

func main() {
	if len(os.Args) < 2 {
		Usage()
		os.Exit(0)
	}

	gopath := os.Getenv("GOPATH")
	if !checkPath(gopath) {
		fmt.Printf("GOPATH('%s') must be a single valid folder\n", gopath)
		os.Exit(1)
	}

	stdLibs = getStdLibs()

	var gitCache string
	var ignoreMissing bool
	var getTestDeps bool

	for _, arg := range os.Args[1:] {
		if arg == "-v" || arg == "--verbose" {
			verbose = true
		} else if strings.HasPrefix(arg, "--git-cache=") {
			var err error
			gitCache, err = filepath.Abs(strings.Split(arg, "=")[1])
			if err != nil {
				panic(err)
			}
		} else if arg == "-i" || arg == "--install" {
			goInstall = true
		} else if arg == "-t" || arg == "--tests" {
			getTestDeps = true
		} else if arg == "--ignore-missing" {
			ignoreMissing = true
		}
	}

	for _, arg := range os.Args[1:] {
		switch arg {
		case "status":
			deps, testDeps := allDeps(gopath, getProjects(gopath))
			Status(deps, testDeps)
			os.Exit(0)

		case "snapshot":
			deps, testDeps := allDeps(gopath, getProjects(gopath))
			depsSnapshot, depsOk := Snapshot(deps)
			testDepsSnapshot, testDepsOk := Snapshot(testDeps)

			if depsOk && testDepsOk {
				writeJson("gsdeps.json", depsSnapshot, testDepsSnapshot)
				os.Exit(0)
			} else {
				os.Exit(1)
			}

		case "update":
			deps, testDeps := readJson("gsdeps.json", ignoreMissing)
			UpdateAll(gopath, deps, gitCache)
			if getTestDeps {
				UpdateAll(gopath, testDeps, gitCache)
			}
			os.Exit(0)

		case "fetch":
			deps, testDeps := readJson("gsdeps.json", ignoreMissing)
			CloneMissing(gopath, deps, gitCache)
			if getTestDeps {
				CloneMissing(gopath, testDeps, gitCache)
			}
			os.Exit(0)

		case "reproduce":
			deps, testDeps := readJson("gsdeps.json", ignoreMissing)
			Reproduce(gopath, deps)
			if getTestDeps {
				Reproduce(gopath, testDeps)
			}
			os.Exit(0)
		}
	}
	Usage()
	os.Exit(1)
}
