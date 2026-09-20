function r(t){const n=t==null?"":String(t);return/[",\n\r]/.test(n)?`"${n.replace(/"/g,'""')}"`:n}function c(t){if(t.length===0)return"";const n=Object.keys(t[0]),e=[n.join(",")];for(const o of t)e.push(n.map(s=>r(o[s])).join(","));return e.join(`\r
`)}self.onmessage=t=>{if(t.data?.type!=="export")return;const n=c(t.data.rows);self.postMessage({type:"export:done",csv:n})};
