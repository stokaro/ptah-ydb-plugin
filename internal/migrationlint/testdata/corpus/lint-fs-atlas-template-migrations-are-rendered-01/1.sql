{{- if eq .Env "dev" }}
CREATE TABLE dev1 (id INT);
{{- else }}
DROP TABLE prod1;
{{- end }}
