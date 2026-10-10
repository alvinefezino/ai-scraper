function setPrompt(t){
  const el=document.getElementById('prompt');
  if(el){ el.value=t; el.focus(); }
}
function fillDemo(){
  const u=document.getElementById('url');
  const p=document.getElementById('prompt');
  if(u) u.value='https://books.toscrape.com/';
  if(p) p.value='Extract all books with title, price, rating and availability.';
  window.scrollTo({top:0, behavior:'smooth'});
  setTimeout(()=>{ if(u) u.focus(); }, 300);
}
function switchTab(name){
  document.querySelectorAll('#resultTabs .tab').forEach(b=>{
    b.classList.toggle('active', b.dataset.tab===name);
  });
  document.querySelectorAll('.tab-panel').forEach(p=> p.classList.remove('active'));
  const panel=document.getElementById('panel-'+name);
  if(panel) panel.classList.add('active');
}
function toast(msg){
  const t=document.getElementById('toast');
  if(!t) return;
  t.textContent=msg;
  t.classList.add('show');
  clearTimeout(t._tm);
  t._tm=setTimeout(()=> t.classList.remove('show'), 2200);
}
async function copyJSON(id){
  const ta=document.getElementById('jsonData-'+id);
  if(!ta){ toast('Nothing to copy'); return; }
  try{ await navigator.clipboard.writeText(ta.value); toast('JSON copied to clipboard'); }
  catch{
    ta.style.display='block'; ta.select(); document.execCommand('copy'); ta.style.display='none';
    toast('JSON copied');
  }
}
async function copyText(text, msg){
  try{ await navigator.clipboard.writeText(text); toast(msg||'Copied'); }
  catch{
    const ta=document.createElement('textarea'); ta.value=text; ta.style.position='fixed'; ta.style.opacity='0';
    document.body.appendChild(ta); ta.select(); document.execCommand('copy'); ta.remove(); toast(msg||'Copied');
  }
}
async function copyColor(hex){
  await copyText(hex, hex+' copied');
}
window.__scraperInit = function(){
  // make tab buttons work after htmx swap
  document.querySelectorAll('#resultTabs .tab').forEach(b=>{
    b.onclick = ()=> switchTab(b.dataset.tab);
  });
};
// htmx events
document.addEventListener('htmx:afterSwap', function(e){
  if(e.detail && e.detail.target && e.detail.target.id==='results'){
    if(window.__scraperInit) window.__scraperInit();
    // scroll to results
    const r=document.getElementById('results');
    if(r) r.scrollIntoView({behavior:'smooth', block:'start'});
  }
});
document.addEventListener('htmx:responseError', function(e){
  toast('Request failed. Check URL and try again');
});
