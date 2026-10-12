mermaid 11.17.2
https://www.npmjs.com/package/mermaid

更新手順（<版> は入れる版。例: 11.17.2）:
  npm pack mermaid@<版>
  tar xzf mermaid-*.tgz
  cp package/dist/mermaid.min.js web/static/vendor/mermaid.min.js
  このファイルの 1 行目の版を <版> に書き換える
