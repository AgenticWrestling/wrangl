// node shoot.js — regenerates the PNGs from prototype.html (needs playwright)
const {chromium}=require('/tmp/pw/node_modules/playwright');
(async()=>{
 const b=await chromium.launch();
 const shots=[];
 for(const v of ['A','B','C']){
  const base=v==='C'?['inline','inline-ok']:['confirm','confirm-ok'];
  const list=[...(v==='B'?['menu']:[]),...base,'archived','task','list'];
  for(const st of list)shots.push([v,st]);
 }
 for(const [w,h,scale,tag] of [[1280,800,1,'desktop'],[390,844,2,'phone']]){
  const ctx=await b.newContext({viewport:{width:w,height:h},deviceScaleFactor:scale});
    for(const [v,st] of shots){
   const p=await ctx.newPage();
   const [s,ok]=st.split('-');
   await p.goto(`file://${__dirname}/prototype.html?shot#${v}-${s}${ok?'-ok':''}`);
   await p.evaluate(()=>document.fonts.ready); await p.waitForTimeout(400);
   await p.screenshot({path:`variant-${v}-${st}-${tag}.png`});
   await p.close();
  }
 }
 await b.close();
})();
