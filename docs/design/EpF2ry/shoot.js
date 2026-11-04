// node shoot.js — regenerates the PNGs from prototype.html (needs playwright)
const {chromium}=require('/tmp/pw/node_modules/playwright');
(async()=>{const b=await chromium.launch();
 const shots=[];for(const v of ['A','B','C']){shots.push([v,'inbox-all'],[v,'inbox-pnp'],[v,'inbox-wrangl'],[v,'projects-all'],[v,'project-all']);}
 shots.push(['B','inbox-all-open']);
 for(const [w,h,sc,tag] of [[1280,800,1,'desktop'],[390,844,2,'phone']]){
  const ctx=await b.newContext({viewport:{width:w,height:h},deviceScaleFactor:sc});
  for(const [v,st] of shots){const p=await ctx.newPage();
   await p.goto(`file://${__dirname}/prototype.html?shot#${v}-${st.replace('-all-open','-all-open')}`);
   await p.evaluate(()=>{document.body.classList.add('shot')});await p.evaluate(()=>document.fonts.ready);await p.waitForTimeout(300);
   await p.screenshot({path:`variant-${v}-${st}-${tag}.png`});await p.close();}}
 await b.close()})();
