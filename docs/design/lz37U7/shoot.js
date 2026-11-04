// node shoot.js — regenerates the PNGs from prototype.html (needs playwright)
const {chromium}=require('/tmp/pw/node_modules/playwright');
(async()=>{const b=await chromium.launch();
 const shots=[['A',''],['A','held'],['B',''],['B','held'],['C',''],['C','sheet']];
 for(const [w,h,sc,tag] of [[1280,800,1,'desktop'],[390,844,2,'phone']]){
  const ctx=await b.newContext({viewport:{width:w,height:h},deviceScaleFactor:sc});
  for(const [v,s] of shots){if(tag==='phone'&&s==='held')continue;const p=await ctx.newPage();
   await p.goto(`file://${__dirname}/prototype.html#${v}${s?'-'+s:''}`);
   if(s==='held'){await p.hover('#recent .r:nth-child(3)');await p.evaluate(()=>activity());}
   await p.evaluate(()=>document.fonts.ready);await p.waitForTimeout(400);
   await p.screenshot({path:`variant-${v}${s?'-'+s:''}-${tag}.png`});await p.close();}
 }await b.close()})();
