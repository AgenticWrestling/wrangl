// node shoot.js — regenerates PNGs from prototype.html (needs playwright)
const {chromium}=require('/tmp/pw/node_modules/playwright');
(async()=>{const b=await chromium.launch();
 const shots=[['A','open'],['B','open'],['C','open'],['C','chord'],['A','pick'],['B','filter']];
 for(const [w,h,sc,tag] of [[1280,800,1,'desktop'],[390,844,2,'phone']]){
  const ctx=await b.newContext({viewport:{width:w,height:h},deviceScaleFactor:sc});
  for(const [v,s] of shots){if(tag==='phone'&&s!=='open')continue;const p=await ctx.newPage();
   await p.goto(`file://${__dirname}/prototype.html#${v}-${s}`);
   await p.evaluate(()=>document.fonts.ready);await p.waitForTimeout(300);
   await p.screenshot({path:`variant-${v}-${s}-${tag}.png`});await p.close();}
 }await b.close()})();
