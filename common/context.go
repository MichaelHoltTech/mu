package common

import (
	bufio "bufio"
	"bytes"
	"fmt"
	"gopkg.in/yaml.v2"
	"io"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

var version string

// GetVersion returns the current version of the app
func GetVersion() string {
	if version == Empty {
		return DefaultVersion
	}
	return version
}

// SetVersion returns the current version of the app
func SetVersion(v string) {
	version = v
}

// NewContext create a new context object
func NewContext() *Context {
	ctx := new(Context)
	return ctx
}

// InitializeConfigFromFile loads config from file
func (ctx *Context) InitializeConfigFromFile(muFile string) error {
	absMuFile, err := filepath.Abs(muFile)

	// set the basedir
	ctx.Config.Basedir = path.Dir(absMuFile)
	log.Debugf("Setting basedir=%s", ctx.Config.Basedir)

	ctx.Config.Repo.Name = path.Base(ctx.Config.Basedir)
	ctx.Config.Repo.Revision = time.Now().Format("20060102150405")

	ctx.Config.RelMuFile, err = getRelMuFile(absMuFile)
	if err != nil {
		return err
	}

	// Get the git revision from the .git folder
	gitRevision, err := findGitRevision(ctx.Config.Basedir)
	if err == nil {
		ctx.Config.Repo.Revision = gitRevision
		gitURL, err := findGitRemoteURL(ctx.Config.Basedir)
		if err == nil {
			gitProvider, gitSlug, err := findGitSlug(gitURL)
			if err == nil {
				ctx.Config.Repo.Provider = gitProvider
				ctx.Config.Repo.Slug = gitSlug
			} else {
				log.Warningf("Unable to determine git slug: %s", err.Error())
			}
		} else {
			log.Warningf("Unable to determine git remote url: %s", err.Error())
		}

		gitBranch, err := findGitBranch(ctx.Config.Basedir)
		if err == nil {
			ctx.Config.Repo.Branch = gitBranch
		} else {
			log.Warningf("Unable to determine git branch: %s", err.Error())
		}
	} else {

		// The .git folder does not exist, check to see if we are in CodeBuild
		if os.Getenv("CODEBUILD_INITIATOR") != "" {
			log.Debugf("Trying to determine git revision from CodeBuild initiator.")
			initiator := os.Getenv("CODEBUILD_INITIATOR")
			parts := strings.Split(initiator, "/")

			// See if the build was initiated by CodePipeline
			if parts[0] == "codepipeline" {
				// Try retrieving the revision from the CodePipeline status
				gitInfo, err := ctx.LocalPipelineManager.GetGitInfo(parts[1])
				if err != nil {
					log.Warningf("Unable to determine git information from CodeBuild initiator '%s': %s", initiator, err)
				}

				sourceVersion := os.Getenv("CODEBUILD_RESOLVED_SOURCE_VERSION")
				if sourceVersion == "" {
					sourceVersion = gitInfo.Revision
				}
				if len(sourceVersion) > 7 {
					ctx.Config.Repo.Revision = string(sourceVersion[:7])
				}

				ctx.Config.Repo.Name = gitInfo.RepoName
				ctx.Config.Repo.Slug = gitInfo.Slug
				ctx.Config.Repo.Provider = gitInfo.Provider
			} else {
				log.Warningf("Unable to process CodeBuild initiator: %s", initiator)
			}
		} else {
			log.Warningf("Unable to determine git revision: %s", err.Error())
		}
	}
	log.Debugf("Setting repo provider=%s", ctx.Config.Repo.Provider)
	log.Debugf("Setting repo name=%s", ctx.Config.Repo.Name)
	log.Debugf("Setting repo revision=%s", ctx.Config.Repo.Revision)
	log.Debugf("Setting repo slug=%s", ctx.Config.Repo.Slug)

	// load yaml config
	yamlFile, err := os.Open(absMuFile)
	if err != nil {
		return err
	}
	defer func() {
		yamlFile.Close()
	}()

	scanner := bufio.NewScanner(yamlFile)
	return ctx.InitializeConfig(&EnvironmentVariableEvaluator{Scanner: *scanner})
}

func getRelMuFile(absMuFile string) (string, error) {
	var repoDir string
	gitDir, error := findGitDirectory(absMuFile)
	if error != nil {
		repoDir, error = os.Getwd()
		if error != nil {
			return "", error
		}
	} else {
		repoDir = filepath.Dir(gitDir)
	}

	absRepoDir, error := filepath.Abs(repoDir)
	if error != nil {
		return "", error
	}

	relMuFile, error := filepath.Rel(absRepoDir, absMuFile)
	if error != nil {
		return "", error
	}

	log.Debugf("Absolute repodir: %s", absRepoDir)
	log.Debugf("Absolute mu file: %s", absMuFile)
	log.Debugf("Relative mu file: %s", relMuFile)

	return relMuFile, nil
}

// InitializeConfig loads config object
func (ctx *Context) InitializeConfig(configReader io.Reader) error {

	// load the configuration
	err := loadYamlConfig(&ctx.Config, configReader)
	if err != nil {
		return err
	}

	return nil
}

// InitializeExtensions loads extension objects
func (ctx *Context) InitializeExtensions() error {
	extMgr := ctx.ExtensionsManager
	// load extensions from mu.yml
	for _, extension := range ctx.Config.Extensions {
		if extension.URL != "" {
			u, err := parseAbsURL(extension.URL, ctx.Config.Basedir)
			if err != nil {
				log.Warningf("Unable to load extension '%s': %s", extension.URL, err)
			} else {
				ext, err := newTemplateArchiveExtension(u, ctx.ArtifactManager)
				if err != nil {
					log.Warningf("Unable to load extension '%s': %s", extension.URL, err)
				} else {
					err = extMgr.AddExtension(ext)
					if err != nil {
						log.Warningf("Unable to load extension '%s': %s", extension.URL, err)
					}
				}
			}
		} else if extension.Image != "" {
			log.Warningf("Docker based extensions is not yet supported!")
		}
	}

	// register the stack overrides from within the mu.yml
	for stackName, template := range ctx.Config.Templates {
		ext := newTemplateOverrideExtension(stackName, template)
		err := extMgr.AddExtension(ext)
		if err != nil {
			log.Warningf("Unable to load extension '%s': %s", ext.ID(), err)
		}
	}

	// register the stack parameters from within the mu.yml
	for stackName, parameters := range ctx.Config.Parameters {
		ext := newParameterOverrideExtension(stackName, parameters)
		err := extMgr.AddExtension(ext)
		if err != nil {
			log.Warningf("Unable to load extension '%s': %s", ext.ID(), err)
		}
	}

	// register the stack tags from within the mu.yml
	for stackName, tags := range ctx.Config.Tags {
		ext := newTagOverrideExtension(stackName, tags)
		err := extMgr.AddExtension(ext)
		if err != nil {
			log.Warningf("Unable to load extension '%s': %s", ext.ID(), err)
		}
	}

	return nil
}

// InitializeContext loads manager objects
func (ctx *Context) InitializeContext() error {
	var err error

	// initialize DockerManager
	ctx.DockerManager, err = newClientDockerManager()
	if err != nil {
		return err
	}

	// initialize ExtensionsManager
	ctx.ExtensionsManager, err = newExtensionsManager()
	if err != nil {
		return err
	}

	return nil
}

func loadYamlConfig(config *Config, yamlReader io.Reader) error {
	yamlBuffer := new(bytes.Buffer)
	yamlBuffer.ReadFrom(yamlReader)
	return yaml.Unmarshal(yamlBuffer.Bytes(), config)
}

func parseAbsURL(urlString string, basedir string) (*url.URL, error) {
	u, err := url.Parse(urlString)
	if err != nil {
		return nil, err
	}

	if !u.IsAbs() {
		basedirURL, err := url.Parse(fmt.Sprintf("file://%s/", basedir))
		if err != nil {
			return nil, err
		}
		u = basedirURL.ResolveReference(u)
		log.Debugf("Resolved relative path to '%s' from basedir '%s'", u, basedirURL)
	}
	return u, nil
}

// EnvironmentVariableEvaluator implements an io.Reader
type EnvironmentVariableEvaluator struct {
	Scanner bufio.Scanner
	buf     bytes.Buffer
}

// Read implements the reader interface
func (m *EnvironmentVariableEvaluator) Read(p []byte) (int, error) {
	m.buf.Reset()
	if !m.Scanner.Scan() {
		return 0, io.EOF
	}
	line := m.Scanner.Text() + "\n"
	line = SubstituteEnvironmentVariablesAsString(line)
	n, werr := m.buf.Write([]byte(line))

	copy(p, m.buf.Bytes())
	if werr != nil {
		return n, werr
	}
	return n, nil
}

// SubstituteEnvironmentVariablesAsString performns environment variable substitution according to Issue #209 (Dynamic Variables)
func SubstituteEnvironmentVariablesAsString(input string) string {
	output := input
	pattern, _ := regexp.Compile("\\$\\{env:[a-zA-Z0-9_]*\\}")
	// find first match
	matches := pattern.FindStringIndex(output)
	// as long as there are more ${env:XXX} patterns....
	for len(matches) > 0 {
		//log.Debugf("matches: %v", matches)
		//log.Debugf("matches[%d] = %v", matches[0], output[matches[0]:]);
		//log.Debugf("matches[%d] = %v", matches[1], output[matches[1]:]);

		// grab the name between ${env: and }
		name := output[matches[0]+6 : matches[1]-1]
		// look it up
		value := os.Getenv(name)
		//log.Debugf("value '%v'", value)

		// substitute it
		output = output[0:matches[0]] + value + output[matches[1]:]
		//log.Debugf("output '%v'", output)

		// try to find another match
		matches = pattern.FindStringIndex(output)
	}
	// all done!
	// log.Debugf("output: %s", output)
	return output
}
