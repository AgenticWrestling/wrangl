// node shoot.js — regenerates the PNGs from prototype.html (needs playwright at /tmp/pw)
const {chromium}=require('/tmp/pw/node_modules/playwright');
(async()=>{const b=await chromium.launch();
 const shots=[];
 for(const v of ['A','B','C']){
  shots.push([`${v}-settings`,`variant-${v}-1-settings`]);
  shots.push([`${v}-editor-bad`,`variant-${v}-2-editor-errors`]);
  shots.push([`${v}-project`,`variant-${v}-3-project-card`]);
  shots.push([`${v}-run`,`variant-${v}-4-run-audit`]);
 }
 shots.push(['A-settings-del','variant-A-5-delete-confirm']);
 for(const [w,h,sc,tag] of [[1280,800,1,'desktop'],[390,844,2,'phone']]){
  const ctx=await b.newContext({viewport:{width:w,height:h},deviceScaleFactor:sc});
  for(const [hash,name] of shots){const p=await ctx.newPage();
   await p.goto(`file://${__dirname}/prototype.html?shot#${hash}`);
   await p.evaluate(()=>document.fonts.ready);await p.waitForTimeout(250);
   await p.screenshot({path:`${name}-${tag}.png`,fullPage:true});await p.close();}
 }await b.close()})();
