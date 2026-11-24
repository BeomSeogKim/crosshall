package setup

// installers registers each adapter bundle's installer by adapter ID. A new
// adapter adds its installer file and its row here; the generic planner does
// not change.
var installers = map[string]installer{
	"claude": claudeInstaller{},
	"codex":  codexInstaller{},
	"agy":    agyInstaller{},
}
