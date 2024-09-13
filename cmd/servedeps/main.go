package main

import (
	"flag"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"net/url"
	"strings"
	"tapfs"
)

type action struct {
	ID string

	Inputs  []*artifact
	Outputs []*artifact

	Orig *tapfs.JSONOpenData
}

type artifact struct {
	ID        string
	ReadBy    []*action
	WrittenBy []*action
}

type problem struct {
	Artifact *artifact
	Actions  []*action

	Desc string
}

func (g *graph) findOverlappingWrites() {
	for _, v := range g.Artifacts {
		if len(v.WrittenBy) > 1 && !strings.HasSuffix(v.ID, ".Tpo") {
			log.Printf("%v", v)
			g.Problems = append(g.Problems, &problem{
				Artifact: v,
				Actions:  v.WrittenBy,
				Desc:     "artifact written by multiple actions",
			})
		}
	}
}

var templates = map[string]string{
	"actionShort": `<a href="/action?id={{.ID}}">{{.ID}}: <pre>{{.Orig.Command}}</pre></a>`,
	"actionsRoot": `<html lang="en">
<body>
  {{template "actions"}}
</body>
</html>`,
	"actions": `<h1>Actions</h1>
  {{range $key, $value := .}}
  <ul>
    <li><a href="/action?id={{$key}}">{{$key}}</a></li>
  </ul>
  {{end}}
`,
	"artifactsRoot": `<html lang="en">
<body>
{{template "artifacts" .}}
</body>
</html>`,
	"artifacts": `  <h1>Artifacts</h1>
  {{range $key, $value := .}}
  <ul>
    <li><a href="/artifact?id={{$key}}">{{$key}}</a></li>
  </ul>
  {{end}}
<p>
`,
	"problems": `  <h1>Problems</h1>
  {{range .}}
  <ul>
    <li><a href="/artifact?id={{.Artifact.ID}}">{{.Artifact.ID}}</a> {{.Desc}}</li>
  </ul>
  {{end}}
<p>
`,
	"problemShort": "{{.Desc}}",
	"artifact": `<html lang="en">
<body>
  <h1>{{.ID}}</h1>
  Read by:
  <ul>
    {{range .ReadBy}}
 	<li>{{template "actionShort" .}}</li>
    {{end}}
  </ul>
<p>
  Written by:
  <ul>
    {{range .WrittenBy}}
 	<li>{{template "actionShort" .}}</li>
    {{end}}
  </ul>
</body>
</html>`,
	"action": `
<body>
  <h1>{{.ID}}</h1>
  Command:
<pre style="white-space: pre-wrap">
  {{.Orig.Command}}
</pre>
  Dir: <pre>{{.Orig.Dir}}</pre>
  Inputs:
  <ul>
    {{range .Inputs}}
    <li><a href="/artifact?id={{.ID}}">{{.ID}}</a></li>
    {{end}}
  </ul>
  Outputs:
  <ul>
    {{range .Outputs}}
    <li><a href="/artifact?id={{.ID}}">{{.ID}}</a></li>
   {{end}}
  </ul>
</body>
</html>`,
	"graphRoot": `
<html lang="en">
<body>
  {{template "artifacts" .Artifacts}}
  {{template "actions" .Actions}}
  {{template "problems" .Problems}}
</body>
</html>`,
}

var tpl *template.Template

func init() {
	tpl = template.New("top")
	for k, v := range templates {
		template.Must(tpl.New(k).Parse(v))
	}
}

type graph struct {
	Artifacts map[string]*artifact
	Actions   map[string]*action
	Problems  []*problem
}

func (g *graph) serveArtifactErr(w http.ResponseWriter, req *http.Request) error {
	vs, err := url.ParseQuery(req.URL.RawQuery)
	if err != nil {
		return fmt.Errorf("ParseQuery")
	}

	if len(vs["id"]) == 0 {
		return fmt.Errorf("id missign")
	}

	id := vs["id"][0]
	art := g.Artifacts[id]
	if art == nil {
		return fmt.Errorf("id unknown")
	}

	return tpl.Lookup("artifact").Execute(w, art)

}

func (g *graph) serveActionErr(w http.ResponseWriter, req *http.Request) error {
	vs, err := url.ParseQuery(req.URL.RawQuery)
	if err != nil {
		return fmt.Errorf("ParseQuery")
	}

	if len(vs["id"]) == 0 {
		return fmt.Errorf("id missign")
	}

	id := vs["id"][0]
	act := g.Actions[id]
	if act == nil {
		return fmt.Errorf("id unknown")
	}
	return tpl.Lookup("action").Execute(w, act)
}

func (g *graph) serveArtifact(w http.ResponseWriter, req *http.Request) {
	if err := g.serveArtifactErr(w, req); err != nil {
		http.Error(w, fmt.Sprintf("Error: %v", err), 400)
	}
}

func (g *graph) serveAction(w http.ResponseWriter, req *http.Request) {
	if err := g.serveActionErr(w, req); err != nil {
		http.Error(w, fmt.Sprintf("Error: %v", err), 400)
	}
}

func (g *graph) serveActionCollection(w http.ResponseWriter, req *http.Request) {
	tpl.Lookup("actionsRoot").Execute(w, g.Actions)
}

func (g *graph) serveArtifactCollection(w http.ResponseWriter, req *http.Request) {
	tpl.Lookup("artifactsRoot").Execute(w, g.Artifacts)
}

func (g *graph) serveRoot(w http.ResponseWriter, req *http.Request) {
	tpl.Lookup("graphRoot").Execute(w, g)
}

func (g *graph) artifact(nm string) *artifact {
	art := g.Artifacts[nm]
	if art == nil {
		art = &artifact{ID: nm}
		g.Artifacts[nm] = art
	}
	return art
}

func (g *graph) add(d *tapfs.JSONOpenData) {
	a, ok := g.Actions[d.ID]
	if !ok {
		a = &action{ID: d.ID, Orig: d}
		g.Actions[d.ID] = a
	}
	for _, nm := range d.Read {
		art := g.artifact(nm)
		art.ReadBy = append(art.ReadBy, a)
		a.Inputs = append(a.Inputs, art)
	}
	for _, nm := range d.Update {
		art := g.artifact(nm)
		art.WrittenBy = append(art.WrittenBy, a)
		a.Outputs = append(a.Outputs, art)
	}
	for _, nm := range d.Create {
		art := g.artifact(nm)
		art.WrittenBy = append(art.WrittenBy, a)
		a.Outputs = append(a.Outputs, art)
	}
	for _, nm := range d.Delete {
		art := g.artifact(nm)
		art.WrittenBy = append(art.WrittenBy, a)
		a.Inputs = append(a.Inputs, art)
	}
}

func main() {
	addr := flag.String("http", ":6710", "where to serve")
	flag.Parse()

	if len(flag.Args()) != 1 {
		log.Fatal("must specify dir as arg")
	}
	data, err := tapfs.Readdir(flag.Arg(0))
	if err != nil {
		log.Fatal(err)
	}

	gr := graph{
		Actions:   map[string]*action{},
		Artifacts: map[string]*artifact{},
	}
	for i := range data {
		gr.add(&data[i])
	}
	gr.findOverlappingWrites()

	http.HandleFunc("/artifact", gr.serveArtifact)
	http.HandleFunc("/artifacts", gr.serveArtifactCollection)
	http.HandleFunc("/action", gr.serveAction)
	http.HandleFunc("/actions", gr.serveActionCollection)
	http.HandleFunc("/", gr.serveRoot)
	log.Printf("serving on %s", *addr)
	log.Fatal(http.ListenAndServe(*addr, nil))
}
