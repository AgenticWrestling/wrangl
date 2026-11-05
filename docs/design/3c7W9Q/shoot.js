// node shoot.js — regenerates the PNGs from prototype.html (needs playwright)
const {chromium}=require('/tmp/pw/node_modules/playwright');
(async()=>{const b=await chromium.launch();
 for(const [w,h,scale,tag] of [[1280,620,1,'desktop'],[390,844,2,'phone']]){
  const ctx=await b.newContext({viewport:{width:w,height:h},deviceScaleFactor:scale});
  for(const v of ['A','B','C']){const p=await ctx.newPage();
   p.on('console',m=>{if(m.type()==='assert')console.log('ASSERT FAILED',m.text())});
   await p.goto(`file://${__dirname}/prototype.html?shot#${v}`);
   await p.waitForTimeout(1000);
   await p.screenshot({path:`variant-${v}-${tag}.png`,fullPage:tag==='phone'});await p.close();}
 }await b.close();})();
